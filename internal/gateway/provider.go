package gateway

import (
	"time"

	"drydock/internal/creds"
)

// Provider issues creds.Grants backed by gateway tokens. The real key never
// leaves the host; the VM only ever sees a bearer token + the base URL.
type Provider struct {
	GW      *Gateway
	Vendor  string
	BaseURL string        // e.g. http://192.168.64.1:8088
	Budget  float64       // default budget when Mint's arg is 0
	TTL     time.Duration // safety-net expiry (= task timeout + margin)
}

type grant struct {
	gw      *Gateway
	token   string
	baseURL string
	vendor  string
}

func (p *Provider) Mint(budgetUSD float64) (creds.Grant, error) {
	b := budgetUSD
	if b == 0 {
		b = p.Budget
	}
	ttl := p.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	tok, err := p.GW.Mint(p.Vendor, b, ttl)
	if err != nil {
		return nil, err
	}
	return &grant{gw: p.GW, token: tok, baseURL: p.BaseURL, vendor: p.Vendor}, nil
}

func (g *grant) EnvVars() []string {
	switch g.vendor {
	case "openai":
		return []string{
			"OPENAI_BASE_URL=" + g.baseURL,
			"OPENAI_API_KEY=" + g.token,
		}
	default: // anthropic
		return []string{
			"ANTHROPIC_BASE_URL=" + g.baseURL,
			"ANTHROPIC_AUTH_TOKEN=" + g.token,
		}
	}
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
