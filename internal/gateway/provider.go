package gateway

import (
	"fmt"
	"time"

	"drydock/internal/creds"
)

// Provider issues creds.Grants backed by gateway tokens. The real key never
// leaves the host; the VM only ever sees a bearer token + the base URL.
type Provider struct {
	GW          *Gateway
	Vendor      string
	BaseURL     string        // e.g. http://192.168.64.1:8088
	BaseURLEnv  string        // env var name injected into VM for the base URL
	TokenEnv    string        // env var name injected into VM for the token
	Budget      float64       // default budget when Mint's arg is 0
	TTL         time.Duration // safety-net expiry (= task timeout + margin)
	MaxRequests int           // 0 = unlimited; primary runaway cap for subscription auth
}

type grant struct {
	gw         *Gateway
	token      string
	baseURL    string
	baseURLEnv string
	tokenEnv   string
}

func (p *Provider) Mint(budgetUSD float64) (creds.Grant, error) {
	if p.BaseURLEnv == "" || p.TokenEnv == "" {
		return nil, fmt.Errorf("gateway: provider %q has empty BaseURLEnv/TokenEnv", p.Vendor)
	}
	b := budgetUSD
	if b == 0 {
		b = p.Budget
	}
	ttl := p.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	tok, err := p.GW.Mint(p.Vendor, b, p.MaxRequests, ttl)
	if err != nil {
		return nil, err
	}
	return &grant{gw: p.GW, token: tok, baseURL: p.BaseURL, baseURLEnv: p.BaseURLEnv, tokenEnv: p.TokenEnv}, nil
}

func (g *grant) EnvVars() []string {
	return []string{g.baseURLEnv + "=" + g.baseURL, g.tokenEnv + "=" + g.token}
}

func (g *grant) Revoke() error {
	g.gw.Revoke(g.token)
	return nil
}

func (g *grant) Spent() float64 {
	s := g.gw.spent(g.token)
	if s < 0 {
		return 0 // lease already revoked/expired
	}
	return s
}
