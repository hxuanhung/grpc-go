/*
 *
 * Copyright 2019 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package balancer

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	structpb "github.com/golang/protobuf/ptypes/struct"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/xds/internal"
	"google.golang.org/grpc/xds/internal/balancer/lrs"
	xdsclient "google.golang.org/grpc/xds/internal/client"
	cdspb "google.golang.org/grpc/xds/internal/proto/envoy/api/v2/cds"
	basepb "google.golang.org/grpc/xds/internal/proto/envoy/api/v2/core/base"
	discoverypb "google.golang.org/grpc/xds/internal/proto/envoy/api/v2/discovery"
	edspb "google.golang.org/grpc/xds/internal/proto/envoy/api/v2/eds"
	adsgrpc "google.golang.org/grpc/xds/internal/proto/envoy/service/discovery/v2/ads"
)

const (
	cdsType          = "type.googleapis.com/envoy.api.v2.Cluster"
	edsType          = "type.googleapis.com/envoy.api.v2.ClusterLoadAssignment"
	endpointRequired = "endpoints_required"
)

// client is responsible for connecting to the specified traffic director, passing the received
// ADS response from the traffic director, and sending notification when communication with the
// traffic director is lost.
type client struct {
	ctx              context.Context
	cancel           context.CancelFunc
	cli              adsgrpc.AggregatedDiscoveryServiceClient
	dialer           func(context.Context, string) (net.Conn, error)
	channelzParentID int64
	enableCDS        bool
	newADS           func(ctx context.Context, resp proto.Message) error
	loseContact      func(ctx context.Context)
	cleanup          func()
	backoff          backoff.Strategy

	loadStore      lrs.Store
	loadReportOnce sync.Once
	config         *xdsclient.Config

	mu sync.Mutex
	cc *grpc.ClientConn
}

func (c *client) run() {
	c.dial()
	c.makeADSCall()
}

func (c *client) close() {
	c.cancel()
	c.mu.Lock()
	if c.cc != nil {
		c.cc.Close()
	}
	c.mu.Unlock()
	c.cleanup()
}

func (c *client) dial() {
	dopts := []grpc.DialOption{c.config.Creds}
	if c.dialer != nil {
		dopts = append(dopts, grpc.WithContextDialer(c.dialer))
	}
	// Explicitly set pickfirst as the balancer.
	dopts = append(dopts, grpc.WithBalancerName(grpc.PickFirstBalancerName))
	if channelz.IsOn() {
		dopts = append(dopts, grpc.WithChannelzParentID(c.channelzParentID))
	}

	cc, err := grpc.DialContext(c.ctx, c.config.BalancerName, dopts...)
	// Since this is a non-blocking dial, so if it fails, it due to some serious error (not network
	// related) error.
	if err != nil {
		grpclog.Fatalf("xds: failed to dial: %v", err)
	}
	c.mu.Lock()
	select {
	case <-c.ctx.Done():
		cc.Close()
	default:
		// only assign c.cc when xds client has not been closed, to prevent ClientConn leak.
		c.cc = cc
	}
	c.mu.Unlock()
}

func (c *client) newCDSRequest() *discoverypb.DiscoveryRequest {
	cdsReq := &discoverypb.DiscoveryRequest{
		Node:    c.config.NodeProto,
		TypeUrl: cdsType,
	}
	return cdsReq
}

func (c *client) newEDSRequest() *discoverypb.DiscoveryRequest {
	// TODO: Once we change the client to always make a CDS call, we can remove
	// this boolean field from the metadata.
	np := proto.Clone(c.config.NodeProto).(*basepb.Node)
	np.Metadata.Fields[endpointRequired] = &structpb.Value{
		Kind: &structpb.Value_BoolValue{BoolValue: c.enableCDS},
	}

	edsReq := &discoverypb.DiscoveryRequest{
		Node: np,
		// TODO: the expected ResourceName could be in a different format from
		// dial target. (test_service.test_namespace.traffic_director.com vs
		// test_namespace:test_service).
		//
		// The solution today is to always include GrpcHostname in metadata,
		// with the value set to dial target.
		//
		// A future solution could be: always do CDS, get cluster name from CDS
		// response, and use it here.
		// `ResourceNames: []string{c.clusterName},`
		TypeUrl: edsType,
	}
	return edsReq
}

func (c *client) makeADSCall() {
	c.cli = adsgrpc.NewAggregatedDiscoveryServiceClient(c.cc)
	retryCount := 0
	var doRetry bool

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		if doRetry {
			backoffTimer := time.NewTimer(c.backoff.Backoff(retryCount))
			select {
			case <-backoffTimer.C:
			case <-c.ctx.Done():
				backoffTimer.Stop()
				return
			}
			retryCount++
		}

		firstRespReceived := c.adsCallAttempt()
		if firstRespReceived {
			retryCount = 0
			doRetry = false
		} else {
			doRetry = true
		}
		c.loseContact(c.ctx)
	}
}

func (c *client) adsCallAttempt() (firstRespReceived bool) {
	firstRespReceived = false
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	st, err := c.cli.StreamAggregatedResources(ctx, grpc.WaitForReady(true))
	if err != nil {
		grpclog.Infof("xds: failed to initial ADS streaming RPC due to %v", err)
		return
	}
	if c.enableCDS {
		if err := st.Send(c.newCDSRequest()); err != nil {
			// current stream is broken, start a new one.
			grpclog.Infof("xds: ads RPC failed due to err: %v, when sending the CDS request ", err)
			return
		}
	}
	if err := st.Send(c.newEDSRequest()); err != nil {
		// current stream is broken, start a new one.
		grpclog.Infof("xds: ads RPC failed due to err: %v, when sending the EDS request", err)
		return
	}
	expectCDS := c.enableCDS
	for {
		resp, err := st.Recv()
		if err != nil {
			// current stream is broken, start a new one.
			grpclog.Infof("xds: ads RPC failed due to err: %v, when receiving the response", err)
			return
		}
		firstRespReceived = true
		resources := resp.GetResources()
		if len(resources) < 1 {
			grpclog.Warning("xds: ADS response contains 0 resource info.")
			// start a new call as server misbehaves by sending a ADS response with 0 resource info.
			return
		}
		if resp.GetTypeUrl() == cdsType && !c.enableCDS {
			grpclog.Warning("xds: received CDS response in custom plugin mode.")
			// start a new call as we receive CDS response when in EDS-only mode.
			return
		}
		var adsResp ptypes.DynamicAny
		if err := ptypes.UnmarshalAny(resources[0], &adsResp); err != nil {
			grpclog.Warningf("xds: failed to unmarshal resources due to %v.", err)
			return
		}
		switch adsResp.Message.(type) {
		case *cdspb.Cluster:
			expectCDS = false
		case *edspb.ClusterLoadAssignment:
			if expectCDS {
				grpclog.Warningf("xds: expecting CDS response, got EDS response instead.")
				return
			}
		}
		if err := c.newADS(c.ctx, adsResp.Message); err != nil {
			grpclog.Warningf("xds: processing new ADS message failed due to %v.", err)
			return
		}
		// Only start load reporting after ADS resp is received.
		//
		// Also, newADS() will close the previous load reporting stream, so we
		// don't have double reporting.
		c.loadReportOnce.Do(func() {
			if c.loadStore != nil {
				go c.loadStore.ReportTo(c.ctx, c.cc)
			}
		})
	}
}

func newXDSClient(balancerName string, enableCDS bool, opts balancer.BuildOptions, loadStore lrs.Store, newADS func(context.Context, proto.Message) error, loseContact func(ctx context.Context), exitCleanup func()) *client {
	c := &client{
		enableCDS:        enableCDS,
		dialer:           opts.Dialer,
		channelzParentID: opts.ChannelzParentID,
		newADS:           newADS,
		loseContact:      loseContact,
		cleanup:          exitCleanup,
		backoff:          backoff.DefaultExponential,
		loadStore:        loadStore,
	}

	c.ctx, c.cancel = context.WithCancel(context.Background())

	// It is possible that NewConfig returns a Config object with certain
	// fields left unspecified. If so, we need to use some sane defaults here.
	c.config = xdsclient.NewConfig()
	if c.config.BalancerName == "" {
		c.config.BalancerName = balancerName
	}
	if c.config.Creds == nil {
		c.config.Creds = credsFromDefaults(balancerName, &opts)
	}
	if c.config.NodeProto == nil {
		c.config.NodeProto = &basepb.Node{
			Metadata: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					internal.GrpcHostname: {
						Kind: &structpb.Value_StringValue{StringValue: opts.Target.Endpoint},
					},
				},
			},
		}
	}
	return c
}

func credsFromDefaults(balancerName string, opts *balancer.BuildOptions) grpc.DialOption {
	if opts.DialCreds == nil {
		grpclog.Warning("xds: no credentials available, using Insecure")
		return grpc.WithInsecure()
	}
	if err := opts.DialCreds.OverrideServerName(balancerName); err != nil {
		grpclog.Warningf("xds: failed to override the server name in credentials: %v, using Insecure", err)
		return grpc.WithInsecure()
	}
	return grpc.WithTransportCredentials(opts.DialCreds)
}
