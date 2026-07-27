package gateway

import (
	"testing"
	"time"
)

// The broker's metrics row needs the number of requests a task actually made.
// The lease already counts admits; this verifies the grant surfaces it.
func TestGrantRequests_SurfacesLeaseCount(t *testing.T) {
	g, err := New(Backend{Vendor: AnthropicVendor(), Cred: StaticKey("REAL")})
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{GW: g, Vendor: "anthropic", BaseURL: "http://gw",
		BaseURLEnv: "ANTHROPIC_BASE_URL", TokenEnv: "ANTHROPIC_AUTH_TOKEN",
		Budget: 1, TTL: time.Minute}
	cg, err := p.Mint(0)
	if err != nil {
		t.Fatal(err)
	}
	rc, ok := cg.(interface{ Requests() int })
	if !ok {
		t.Fatal("gateway grant does not implement Requests() int")
	}
	if got := rc.Requests(); got != 0 {
		t.Fatalf("fresh grant Requests()=%d, want 0", got)
	}

	// Find the minted token and simulate two admitted requests the way the
	// gateway itself does (admit() increments Lease.Requests).
	g.mu.Lock()
	for _, l := range g.leases {
		l.Requests = 2
	}
	g.mu.Unlock()
	if got := rc.Requests(); got != 2 {
		t.Fatalf("Requests()=%d, want 2", got)
	}

	// After revoke the lease is gone; report 0, never -1.
	_ = cg.Revoke()
	if got := rc.Requests(); got != 0 {
		t.Fatalf("Requests() after revoke=%d, want 0", got)
	}
}
