// Package gateway is an in-broker reverse proxy in front of api.anthropic.com.
// The VM authenticates with a per-task bearer token; the gateway holds the real
// key (never exposed to the VM), swaps it in, and meters usage against a budget.
package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

type Lease struct {
	BudgetUSD float64
	SpentUSD  float64
	Expiry    time.Time
}

type Gateway struct {
	mu       sync.Mutex
	leases   map[string]*Lease
	realKey  string
	upstream *url.URL
	prices   map[string]Price
	proxy    *httputil.ReverseProxy
}

type ctxKey struct{}

func New(realKey, upstream string, prices map[string]Price) (*Gateway, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	g := &Gateway{
		leases:   map[string]*Lease{},
		realKey:  realKey,
		upstream: u,
		prices:   prices,
	}
	g.proxy = &httputil.ReverseProxy{Director: g.director, ModifyResponse: g.meter}
	return g, nil
}

func (g *Gateway) Mint(budgetUSD float64, ttl time.Duration) string {
	b := make([]byte, 18)
	rand.Read(b)
	tok := "tok_" + hex.EncodeToString(b)
	g.mu.Lock()
	g.leases[tok] = &Lease{BudgetUSD: budgetUSD, Expiry: time.Now().Add(ttl)}
	g.mu.Unlock()
	return tok
}

func (g *Gateway) Revoke(token string) {
	g.mu.Lock()
	delete(g.leases, token)
	g.mu.Unlock()
}

func (g *Gateway) spent(token string) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := g.leases[token]; l != nil {
		return l.SpentUSD
	}
	return -1
}

// check returns (lease, 0) when usable, or (nil, statusCode) to reject.
func (g *Gateway) check(token string) (*Lease, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.leases[token]
	if l == nil || time.Now().After(l.Expiry) {
		return nil, http.StatusUnauthorized
	}
	if l.SpentUSD >= l.BudgetUSD {
		return nil, http.StatusPaymentRequired
	}
	return l, 0
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := ""
	if a := r.Header.Get("Authorization"); len(a) > 7 && a[:7] == "Bearer " {
		tok = a[7:]
	}
	lease, status := g.check(tok)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	ctx := contextWith(r, lease)
	g.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (g *Gateway) director(req *http.Request) {
	req.URL.Scheme = g.upstream.Scheme
	req.URL.Host = g.upstream.Host
	req.Host = g.upstream.Host
	req.Header.Del("Authorization")
	req.Header.Set("X-Api-Key", g.realKey)
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
}

// meter tees the response body and, on completion, adds its cost to the lease.
func (g *Gateway) meter(resp *http.Response) error {
	lease, _ := resp.Request.Context().Value(ctxKey{}).(*Lease)
	if lease == nil {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	resp.Body = &usageReader{rc: resp.Body, onDone: func(body []byte) {
		if model, in, out, ok := parseUsage(body, ct); ok {
			g.mu.Lock()
			lease.SpentUSD += cost(g.prices, model, in, out)
			g.mu.Unlock()
		}
	}}
	return nil
}

// usageReader buffers the streamed body and invokes onDone once at EOF/Close.
type usageReader struct {
	rc     io.ReadCloser
	buf    bytes.Buffer
	onDone func([]byte)
	done   bool
}

func (u *usageReader) Read(p []byte) (int, error) {
	n, err := u.rc.Read(p)
	if n > 0 {
		u.buf.Write(p[:n])
	}
	if err == io.EOF {
		u.finish()
	}
	return n, err
}

func (u *usageReader) Close() error {
	u.finish()
	return u.rc.Close()
}

func (u *usageReader) finish() {
	if u.done {
		return
	}
	u.done = true
	u.onDone(u.buf.Bytes())
}
