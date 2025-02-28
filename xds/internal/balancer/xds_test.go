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
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	xdspb "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/golang/protobuf/proto"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/internal/grpctest"
	"google.golang.org/grpc/internal/leakcheck"
	"google.golang.org/grpc/resolver"
	xdsinternal "google.golang.org/grpc/xds/internal"
	"google.golang.org/grpc/xds/internal/balancer/lrs"
)

var lbABuilder = &balancerABuilder{}

func init() {
	balancer.Register(&xdsBalancerBuilder{})
	balancer.Register(lbABuilder)
	balancer.Register(&balancerBBuilder{})
}

type s struct{}

func (s) Teardown(t *testing.T) {
	leakcheck.Check(t)
}

func Test(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

const (
	fakeBalancerA = "fake_balancer_A"
	fakeBalancerB = "fake_balancer_B"
)

var (
	testBalancerNameFooBar = "foo.bar"
	testLBConfigFooBar     = &xdsinternal.LBConfig{
		BalancerName:   testBalancerNameFooBar,
		ChildPolicy:    &xdsinternal.LoadBalancingConfig{Name: fakeBalancerB},
		FallBackPolicy: &xdsinternal.LoadBalancingConfig{Name: fakeBalancerA},
	}

	specialAddrForBalancerA = resolver.Address{Addr: "this.is.balancer.A"}
	specialAddrForBalancerB = resolver.Address{Addr: "this.is.balancer.B"}

	// mu protects the access of latestFakeEdsBalancer
	mu                    sync.Mutex
	latestFakeEdsBalancer *fakeEDSBalancer
)

type balancerABuilder struct {
	mu           sync.Mutex
	lastBalancer *balancerA
}

func (b *balancerABuilder) Build(cc balancer.ClientConn, opts balancer.BuildOptions) balancer.Balancer {
	b.mu.Lock()
	b.lastBalancer = &balancerA{cc: cc, subconnStateChange: make(chan *scStateChange, 10)}
	b.mu.Unlock()
	return b.lastBalancer
}

func (b *balancerABuilder) Name() string {
	return string(fakeBalancerA)
}

func (b *balancerABuilder) getLastBalancer() *balancerA {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastBalancer
}

func (b *balancerABuilder) clearLastBalancer() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastBalancer = nil
}

type balancerBBuilder struct{}

func (b *balancerBBuilder) Build(cc balancer.ClientConn, opts balancer.BuildOptions) balancer.Balancer {
	return &balancerB{cc: cc}
}

func (*balancerBBuilder) Name() string {
	return string(fakeBalancerB)
}

// A fake balancer implementation which does two things:
// * Appends a unique address to the list of resolved addresses received before
//   attempting to create a SubConn.
// * Makes the received subConn state changes available through a channel, for
//   the test to inspect.
type balancerA struct {
	cc                 balancer.ClientConn
	subconnStateChange chan *scStateChange
}

func (b *balancerA) HandleSubConnStateChange(sc balancer.SubConn, state connectivity.State) {
	b.subconnStateChange <- &scStateChange{sc: sc, state: state}
}

func (b *balancerA) HandleResolvedAddrs(addrs []resolver.Address, err error) {
	_, _ = b.cc.NewSubConn(append(addrs, specialAddrForBalancerA), balancer.NewSubConnOptions{})
}

func (b *balancerA) Close() {}

// A fake balancer implementation which appends a unique address to the list of
// resolved addresses received before attempting to create a SubConn.
type balancerB struct {
	cc balancer.ClientConn
}

func (b *balancerB) HandleResolvedAddrs(addrs []resolver.Address, err error) {
	_, _ = b.cc.NewSubConn(append(addrs, specialAddrForBalancerB), balancer.NewSubConnOptions{})
}

func (balancerB) HandleSubConnStateChange(sc balancer.SubConn, state connectivity.State) {
	panic("implement me")
}
func (balancerB) Close() {}

func newTestClientConn() *testClientConn {
	return &testClientConn{newSubConns: make(chan []resolver.Address, 10)}
}

type testClientConn struct {
	newSubConns chan []resolver.Address
}

func (t *testClientConn) NewSubConn(addrs []resolver.Address, opts balancer.NewSubConnOptions) (balancer.SubConn, error) {
	t.newSubConns <- addrs
	return nil, nil
}

func (testClientConn) RemoveSubConn(balancer.SubConn)                              {}
func (testClientConn) UpdateBalancerState(s connectivity.State, p balancer.Picker) {}
func (testClientConn) ResolveNow(resolver.ResolveNowOption)                        {}
func (testClientConn) Target() string                                              { return testServiceName }

type scStateChange struct {
	sc    balancer.SubConn
	state connectivity.State
}

type fakeEDSBalancer struct {
	cc                 balancer.ClientConn
	edsChan            chan *xdspb.ClusterLoadAssignment
	childPolicy        chan *xdsinternal.LoadBalancingConfig
	fallbackPolicy     chan *xdsinternal.LoadBalancingConfig
	subconnStateChange chan *scStateChange
	loadStore          lrs.Store
}

func (f *fakeEDSBalancer) HandleSubConnStateChange(sc balancer.SubConn, state connectivity.State) {
	f.subconnStateChange <- &scStateChange{sc: sc, state: state}
}

func (f *fakeEDSBalancer) Close() {
	mu.Lock()
	defer mu.Unlock()
	latestFakeEdsBalancer = nil
}

func (f *fakeEDSBalancer) HandleEDSResponse(edsResp *xdspb.ClusterLoadAssignment) {
	f.edsChan <- edsResp
}

func (f *fakeEDSBalancer) HandleChildPolicy(name string, config json.RawMessage) {
	f.childPolicy <- &xdsinternal.LoadBalancingConfig{
		Name:   name,
		Config: config,
	}
}

func newFakeEDSBalancer(cc balancer.ClientConn, loadStore lrs.Store) edsBalancerInterface {
	lb := &fakeEDSBalancer{
		cc:                 cc,
		edsChan:            make(chan *xdspb.ClusterLoadAssignment, 10),
		childPolicy:        make(chan *xdsinternal.LoadBalancingConfig, 10),
		fallbackPolicy:     make(chan *xdsinternal.LoadBalancingConfig, 10),
		subconnStateChange: make(chan *scStateChange, 10),
		loadStore:          loadStore,
	}
	mu.Lock()
	latestFakeEdsBalancer = lb
	mu.Unlock()
	return lb
}

func getLatestEdsBalancer() *fakeEDSBalancer {
	mu.Lock()
	defer mu.Unlock()
	return latestFakeEdsBalancer
}

type fakeSubConn struct{}

func (*fakeSubConn) UpdateAddresses([]resolver.Address) { panic("implement me") }
func (*fakeSubConn) Connect()                           { panic("implement me") }

// TestXdsFallbackResolvedAddrs verifies that the fallback balancer specified
// in the provided lbconfig is initialized, and that it receives the addresses
// pushed by the resolver.
//
// The test does the following:
// * Builds a new xds balancer.
// * Since there is no xDS server to respond to requests from the xds client
//   (created as part of the xds balancer), we expect the fallback policy to
//   kick in.
// * Repeatedly pushes new ClientConnState which specifies the same fallback
//   policy, but a different set of resolved addresses.
// * The fallback policy is implemented by a fake balancer, which appends a
//   unique address to the list of addresses it uses to create the SubConn.
// * We also have a fake ClientConn which verifies that it receives the
//   expected address list.
func (s) TestXdsFallbackResolvedAddrs(t *testing.T) {
	startupTimeout = 500 * time.Millisecond
	defer func() { startupTimeout = defaultTimeout }()

	builder := balancer.Get(xdsName)
	cc := newTestClientConn()
	b := builder.Build(cc, balancer.BuildOptions{Target: resolver.Target{Endpoint: testServiceName}})
	lb, ok := b.(*xdsBalancer)
	if !ok {
		t.Fatalf("builder.Build() returned a balancer of type %T, want *xdsBalancer", b)
	}
	defer lb.Close()

	tests := []struct {
		resolvedAddrs []resolver.Address
		wantAddrs     []resolver.Address
	}{
		{
			resolvedAddrs: []resolver.Address{{Addr: "1.1.1.1:10001"}, {Addr: "2.2.2.2:10002"}},
			wantAddrs:     []resolver.Address{{Addr: "1.1.1.1:10001"}, {Addr: "2.2.2.2:10002"}, specialAddrForBalancerA},
		},
		{
			resolvedAddrs: []resolver.Address{{Addr: "1.1.1.1:10001"}},
			wantAddrs:     []resolver.Address{{Addr: "1.1.1.1:10001"}, specialAddrForBalancerA},
		},
	}
	for _, test := range tests {
		lb.UpdateClientConnState(balancer.ClientConnState{
			ResolverState:  resolver.State{Addresses: test.resolvedAddrs},
			BalancerConfig: testLBConfigFooBar,
		})

		select {
		case gotAddrs := <-cc.newSubConns:
			if !reflect.DeepEqual(gotAddrs, test.wantAddrs) {
				t.Fatalf("got new subconn address %v, want %v", gotAddrs, test.wantAddrs)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout when getting new subconn result")
		}
	}
}

func (s) TestXdsBalanceHandleBalancerConfigBalancerNameUpdate(t *testing.T) {
	startupTimeout = 500 * time.Millisecond
	originalNewEDSBalancer := newEDSBalancer
	newEDSBalancer = newFakeEDSBalancer
	defer func() {
		startupTimeout = defaultTimeout
		newEDSBalancer = originalNewEDSBalancer
	}()

	builder := balancer.Get(xdsName)
	cc := newTestClientConn()
	lb, ok := builder.Build(cc, balancer.BuildOptions{Target: resolver.Target{Endpoint: testServiceName}}).(*xdsBalancer)
	if !ok {
		t.Fatalf("unable to type assert to *xdsBalancer")
	}
	defer lb.Close()
	addrs := []resolver.Address{{Addr: "1.1.1.1:10001"}, {Addr: "2.2.2.2:10002"}, {Addr: "3.3.3.3:10003"}}
	lb.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolver.State{Addresses: addrs},
		BalancerConfig: testLBConfigFooBar,
	})

	// verify fallback takes over
	select {
	case nsc := <-cc.newSubConns:
		if !reflect.DeepEqual(append(addrs, specialAddrForBalancerA), nsc) {
			t.Fatalf("got new subconn address %v, want %v", nsc, append(addrs, specialAddrForBalancerA))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout when getting new subconn result")
	}

	var cleanups []func()
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()
	// In the first iteration, an eds balancer takes over fallback balancer
	// In the second iteration, a new xds client takes over previous one.
	for i := 0; i < 2; i++ {
		addr, td, _, cleanup := setupServer(t)
		cleanups = append(cleanups, cleanup)
		workingLBConfig := &xdsinternal.LBConfig{
			BalancerName:   addr,
			ChildPolicy:    &xdsinternal.LoadBalancingConfig{Name: fakeBalancerA},
			FallBackPolicy: &xdsinternal.LoadBalancingConfig{Name: fakeBalancerA},
		}
		lb.UpdateClientConnState(balancer.ClientConnState{
			ResolverState:  resolver.State{Addresses: addrs},
			BalancerConfig: workingLBConfig,
		})
		td.sendResp(&response{resp: testEDSResp})

		var j int
		for j = 0; j < 10; j++ {
			if edsLB := getLatestEdsBalancer(); edsLB != nil { // edsLB won't change between the two iterations
				select {
				case gotEDS := <-edsLB.edsChan:
					if !proto.Equal(gotEDS, testClusterLoadAssignment) {
						t.Fatalf("edsBalancer got eds: %v, want %v", gotEDS, testClusterLoadAssignment)
					}
				case <-time.After(time.Second):
					t.Fatal("haven't got EDS update after 1s")
				}
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if j == 10 {
			t.Fatal("edsBalancer instance has not been created or updated after 1s")
		}
	}
}

// switch child policy, lb stays the same
func (s) TestXdsBalanceHandleBalancerConfigChildPolicyUpdate(t *testing.T) {
	originalNewEDSBalancer := newEDSBalancer
	newEDSBalancer = newFakeEDSBalancer
	defer func() {
		newEDSBalancer = originalNewEDSBalancer
	}()

	builder := balancer.Get(xdsName)
	cc := newTestClientConn()
	lb, ok := builder.Build(cc, balancer.BuildOptions{Target: resolver.Target{Endpoint: testServiceName}}).(*xdsBalancer)
	if !ok {
		t.Fatalf("unable to type assert to *xdsBalancer")
	}
	defer lb.Close()

	var cleanups []func()
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()
	for _, test := range []struct {
		cfg                 *xdsinternal.LBConfig
		responseToSend      *xdspb.DiscoveryResponse
		expectedChildPolicy *xdsinternal.LoadBalancingConfig
	}{
		{
			cfg: &xdsinternal.LBConfig{
				ChildPolicy: &xdsinternal.LoadBalancingConfig{
					Name:   fakeBalancerA,
					Config: json.RawMessage("{}"),
				},
			},
			responseToSend: testEDSResp,
			expectedChildPolicy: &xdsinternal.LoadBalancingConfig{
				Name:   string(fakeBalancerA),
				Config: json.RawMessage(`{}`),
			},
		},
		{
			cfg: &xdsinternal.LBConfig{
				ChildPolicy: &xdsinternal.LoadBalancingConfig{
					Name:   fakeBalancerB,
					Config: json.RawMessage("{}"),
				},
			},
			expectedChildPolicy: &xdsinternal.LoadBalancingConfig{
				Name:   string(fakeBalancerB),
				Config: json.RawMessage(`{}`),
			},
		},
	} {
		addr, td, _, cleanup := setupServer(t)
		cleanups = append(cleanups, cleanup)
		test.cfg.BalancerName = addr

		lb.UpdateClientConnState(balancer.ClientConnState{BalancerConfig: test.cfg})
		if test.responseToSend != nil {
			td.sendResp(&response{resp: test.responseToSend})
		}
		var i int
		for i = 0; i < 10; i++ {
			if edsLB := getLatestEdsBalancer(); edsLB != nil {
				select {
				case childPolicy := <-edsLB.childPolicy:
					if !reflect.DeepEqual(childPolicy, test.expectedChildPolicy) {
						t.Fatalf("got childPolicy %v, want %v", childPolicy, test.expectedChildPolicy)
					}
				case <-time.After(time.Second):
					t.Fatal("haven't got policy update after 1s")
				}
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if i == 10 {
			t.Fatal("edsBalancer instance has not been created or updated after 1s")
		}
	}
}

// not in fallback mode, overwrite fallback info.
// in fallback mode, update config or switch balancer.
func (s) TestXdsBalanceHandleBalancerConfigFallBackUpdate(t *testing.T) {
	originalNewEDSBalancer := newEDSBalancer
	newEDSBalancer = newFakeEDSBalancer
	defer func() {
		newEDSBalancer = originalNewEDSBalancer
	}()

	builder := balancer.Get(xdsName)
	cc := newTestClientConn()
	lb, ok := builder.Build(cc, balancer.BuildOptions{Target: resolver.Target{Endpoint: testServiceName}}).(*xdsBalancer)
	if !ok {
		t.Fatalf("unable to type assert to *xdsBalancer")
	}
	defer lb.Close()

	addr, td, _, cleanup := setupServer(t)

	cfg := xdsinternal.LBConfig{
		BalancerName:   addr,
		ChildPolicy:    &xdsinternal.LoadBalancingConfig{Name: fakeBalancerA},
		FallBackPolicy: &xdsinternal.LoadBalancingConfig{Name: fakeBalancerA},
	}
	lb.UpdateClientConnState(balancer.ClientConnState{BalancerConfig: &cfg})

	addrs := []resolver.Address{{Addr: "1.1.1.1:10001"}, {Addr: "2.2.2.2:10002"}, {Addr: "3.3.3.3:10003"}}
	cfg2 := cfg
	cfg2.FallBackPolicy = &xdsinternal.LoadBalancingConfig{Name: fakeBalancerB}
	lb.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolver.State{Addresses: addrs},
		BalancerConfig: &cfg2,
	})

	td.sendResp(&response{resp: testEDSResp})

	var i int
	for i = 0; i < 10; i++ {
		if edsLB := getLatestEdsBalancer(); edsLB != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if i == 10 {
		t.Fatal("edsBalancer instance has not been created and assigned to lb.xdsLB after 1s")
	}

	cleanup()

	// verify fallback balancer B takes over
	select {
	case nsc := <-cc.newSubConns:
		if !reflect.DeepEqual(append(addrs, specialAddrForBalancerB), nsc) {
			t.Fatalf("got new subconn address %v, want %v", nsc, append(addrs, specialAddrForBalancerB))
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout when getting new subconn result")
	}

	cfg3 := cfg
	cfg3.FallBackPolicy = &xdsinternal.LoadBalancingConfig{Name: fakeBalancerA}
	lb.UpdateClientConnState(balancer.ClientConnState{
		ResolverState:  resolver.State{Addresses: addrs},
		BalancerConfig: &cfg3,
	})

	// verify fallback balancer A takes over
	select {
	case nsc := <-cc.newSubConns:
		if !reflect.DeepEqual(append(addrs, specialAddrForBalancerA), nsc) {
			t.Fatalf("got new subconn address %v, want %v", nsc, append(addrs, specialAddrForBalancerA))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout when getting new subconn result")
	}
}

func (s) TestXdsBalancerHandlerSubConnStateChange(t *testing.T) {
	originalNewEDSBalancer := newEDSBalancer
	newEDSBalancer = newFakeEDSBalancer
	defer func() {
		newEDSBalancer = originalNewEDSBalancer
	}()

	builder := balancer.Get(xdsName)
	cc := newTestClientConn()
	lb, ok := builder.Build(cc, balancer.BuildOptions{Target: resolver.Target{Endpoint: testServiceName}}).(*xdsBalancer)
	if !ok {
		t.Fatalf("unable to type assert to *xdsBalancer")
	}
	defer lb.Close()

	addr, td, _, cleanup := setupServer(t)
	defer cleanup()
	cfg := &xdsinternal.LBConfig{
		BalancerName:   addr,
		ChildPolicy:    &xdsinternal.LoadBalancingConfig{Name: fakeBalancerA},
		FallBackPolicy: &xdsinternal.LoadBalancingConfig{Name: fakeBalancerA},
	}
	lb.UpdateClientConnState(balancer.ClientConnState{BalancerConfig: cfg})

	td.sendResp(&response{resp: testEDSResp})

	expectedScStateChange := &scStateChange{
		sc:    &fakeSubConn{},
		state: connectivity.Ready,
	}

	var i int
	for i = 0; i < 10; i++ {
		if edsLB := getLatestEdsBalancer(); edsLB != nil {
			lb.UpdateSubConnState(expectedScStateChange.sc, balancer.SubConnState{ConnectivityState: expectedScStateChange.state})
			select {
			case scsc := <-edsLB.subconnStateChange:
				if !reflect.DeepEqual(scsc, expectedScStateChange) {
					t.Fatalf("got subconn state change %v, want %v", scsc, expectedScStateChange)
				}
			case <-time.After(time.Second):
				t.Fatal("haven't got subconn state change after 1s")
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if i == 10 {
		t.Fatal("edsBalancer instance has not been created and assigned to lb.xdsLB after 1s")
	}

	// lbAbuilder has a per binary record what's the last balanceA created. We need to clear the record
	// to make sure there's a new one created and get the pointer to it.
	lbABuilder.clearLastBalancer()
	cleanup()

	// switch to fallback
	// fallback balancer A takes over
	for i = 0; i < 10; i++ {
		if fblb := lbABuilder.getLastBalancer(); fblb != nil {
			lb.UpdateSubConnState(expectedScStateChange.sc, balancer.SubConnState{ConnectivityState: expectedScStateChange.state})
			select {
			case scsc := <-fblb.subconnStateChange:
				if !reflect.DeepEqual(scsc, expectedScStateChange) {
					t.Fatalf("got subconn state change %v, want %v", scsc, expectedScStateChange)
				}
			case <-time.After(time.Second):
				t.Fatal("haven't got subconn state change after 1s")
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if i == 10 {
		t.Fatal("balancerA instance has not been created after 1s")
	}
}

func (s) TestXdsBalancerFallBackSignalFromEdsBalancer(t *testing.T) {
	originalNewEDSBalancer := newEDSBalancer
	newEDSBalancer = newFakeEDSBalancer
	defer func() {
		newEDSBalancer = originalNewEDSBalancer
	}()

	builder := balancer.Get(xdsName)
	cc := newTestClientConn()
	lb, ok := builder.Build(cc, balancer.BuildOptions{Target: resolver.Target{Endpoint: testServiceName}}).(*xdsBalancer)
	if !ok {
		t.Fatalf("unable to type assert to *xdsBalancer")
	}
	defer lb.Close()

	addr, td, _, cleanup := setupServer(t)
	defer cleanup()
	cfg := &xdsinternal.LBConfig{
		BalancerName:   addr,
		ChildPolicy:    &xdsinternal.LoadBalancingConfig{Name: fakeBalancerA},
		FallBackPolicy: &xdsinternal.LoadBalancingConfig{Name: fakeBalancerA},
	}
	lb.UpdateClientConnState(balancer.ClientConnState{BalancerConfig: cfg})

	td.sendResp(&response{resp: testEDSResp})

	expectedScStateChange := &scStateChange{
		sc:    &fakeSubConn{},
		state: connectivity.Ready,
	}

	var i int
	for i = 0; i < 10; i++ {
		if edsLB := getLatestEdsBalancer(); edsLB != nil {
			lb.UpdateSubConnState(expectedScStateChange.sc, balancer.SubConnState{ConnectivityState: expectedScStateChange.state})
			select {
			case scsc := <-edsLB.subconnStateChange:
				if !reflect.DeepEqual(scsc, expectedScStateChange) {
					t.Fatalf("got subconn state change %v, want %v", scsc, expectedScStateChange)
				}
			case <-time.After(time.Second):
				t.Fatal("haven't got subconn state change after 1s")
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if i == 10 {
		t.Fatal("edsBalancer instance has not been created and assigned to lb.xdsLB after 1s")
	}

	// lbAbuilder has a per binary record what's the last balanceA created. We need to clear the record
	// to make sure there's a new one created and get the pointer to it.
	lbABuilder.clearLastBalancer()
	cleanup()

	// switch to fallback
	// fallback balancer A takes over
	for i = 0; i < 10; i++ {
		if fblb := lbABuilder.getLastBalancer(); fblb != nil {
			lb.UpdateSubConnState(expectedScStateChange.sc, balancer.SubConnState{ConnectivityState: expectedScStateChange.state})
			select {
			case scsc := <-fblb.subconnStateChange:
				if !reflect.DeepEqual(scsc, expectedScStateChange) {
					t.Fatalf("got subconn state change %v, want %v", scsc, expectedScStateChange)
				}
			case <-time.After(time.Second):
				t.Fatal("haven't got subconn state change after 1s")
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if i == 10 {
		t.Fatal("balancerA instance has not been created after 1s")
	}
}

func (s) TestXdsBalancerConfigParsingSelectingLBPolicy(t *testing.T) {
	js := json.RawMessage(`{
"balancerName": "fake.foo.bar",
"childPolicy": [{"fake_balancer_C": {}}, {"fake_balancer_A": {}}, {"fake_balancer_B": {}}],
"fallbackPolicy": [{"fake_balancer_C": {}}, {"fake_balancer_B": {}}, {"fake_balancer_A": {}}]
}`)
	cfg, err := (&xdsBalancerBuilder{}).ParseConfig(js)
	if err != nil {
		t.Fatalf("unable to unmarshal balancer config into xds config: %v", err)
	}
	xdsCfg := cfg.(*xdsinternal.LBConfig)
	wantChildPolicy := &xdsinternal.LoadBalancingConfig{Name: string(fakeBalancerA), Config: json.RawMessage(`{}`)}
	if !reflect.DeepEqual(xdsCfg.ChildPolicy, wantChildPolicy) {
		t.Fatalf("got child policy %v, want %v", xdsCfg.ChildPolicy, wantChildPolicy)
	}
	wantFallbackPolicy := &xdsinternal.LoadBalancingConfig{Name: string(fakeBalancerB), Config: json.RawMessage(`{}`)}
	if !reflect.DeepEqual(xdsCfg.FallBackPolicy, wantFallbackPolicy) {
		t.Fatalf("got fallback policy %v, want %v", xdsCfg.FallBackPolicy, wantFallbackPolicy)
	}
}

func (s) TestXdsLoadbalancingConfigParsing(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want *xdsinternal.LBConfig
	}{
		{
			name: "empty",
			s:    "{}",
			want: &xdsinternal.LBConfig{},
		},
		{
			name: "success1",
			s:    `{"childPolicy":[{"pick_first":{}}]}`,
			want: &xdsinternal.LBConfig{
				ChildPolicy: &xdsinternal.LoadBalancingConfig{
					Name:   "pick_first",
					Config: json.RawMessage(`{}`),
				},
			},
		},
		{
			name: "success2",
			s:    `{"childPolicy":[{"round_robin":{}},{"pick_first":{}}]}`,
			want: &xdsinternal.LBConfig{
				ChildPolicy: &xdsinternal.LoadBalancingConfig{
					Name:   "round_robin",
					Config: json.RawMessage(`{}`),
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg xdsinternal.LBConfig
			if err := json.Unmarshal([]byte(tt.s), &cfg); err != nil || !reflect.DeepEqual(&cfg, tt.want) {
				t.Errorf("test name: %s, parseFullServiceConfig() = %+v, err: %v, want %+v, <nil>", tt.name, cfg, err, tt.want)
			}
		})
	}
}
