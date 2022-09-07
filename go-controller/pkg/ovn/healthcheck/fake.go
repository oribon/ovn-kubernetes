package healthcheck

import (
	"context"
	"net"
	"time"
)

type fakeEgressIPHealthClient struct {
	Connected        bool
	ProbeCount       int
	FakeProbeFailure bool
}

type fakeEgressIPHealthClientAllocator struct{}

func (fehc *fakeEgressIPHealthClient) IsConnected() bool {
	return fehc.Connected
}

func (fehc *fakeEgressIPHealthClient) Connect(dialCtx context.Context, mgmtIPs []net.IP, healthCheckPort int) bool {
	if fehc.FakeProbeFailure {
		return false
	}
	fehc.Connected = true
	return true
}

func (fehc *fakeEgressIPHealthClient) Disconnect() {
	fehc.Connected = false
	fehc.ProbeCount = 0
}

func (fehc *fakeEgressIPHealthClient) Probe(dialCtx context.Context) bool {
	if fehc.Connected && !fehc.FakeProbeFailure {
		fehc.ProbeCount++
		return true
	}
	return false
}

type fakeEgressIPDialer struct{}

func (f fakeEgressIPDialer) dial(ip net.IP, timeout time.Duration) bool {
	return true
}

func (f *fakeEgressIPHealthClientAllocator) allocate(nodeName string) healthcheck.EgressIPHealthClient {
	return &fakeEgressIPHealthClient{}
}
