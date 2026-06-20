// Package gateway is an in-broker reverse proxy in front of api.anthropic.com.
// The VM authenticates with a per-task bearer token; the gateway holds the real
// key (never exposed to the VM), swaps it in, and meters usage against a budget.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

type Lease struct {
	// Token is the value Mint returned. Stored so check() can do a
	// constant-time equality against the bearer the caller presented;
	// even if Go's map lookup ever changes to a timing-sensitive shape,
	// the defense-in-depth comparison stops timing-side-channel leakage.
	Token     string
	Vendor    string
	BudgetUSD float64
	SpentUSD  float64
	Expiry    time.Time
}

type vendorRT struct {
	v        Vendor
	cred     Credential
	upstream *url.URL
}

type reqCtx struct {
	lease  *Lease
	secret string
}

type Gateway struct {
	mu      sync.Mutex
	leases  map[string]*Lease
	vendors map[string]vendorRT
	proxy   *httputil.ReverseProxy
}

type ctxKey struct{}

func New(backends ...Backend) (*Gateway, error) {
	g := &Gateway{leases: map[string]*Lease{}, vendors: map[string]vendorRT{}}
	for _, b := range backends {
		u, err := url.Parse(b.Vendor.BaseURL)
		if err != nil {
			return nil, err
		}
		g.vendors[b.Vendor.Name] = vendorRT{v: b.Vendor, cred: b.Cred, upstream: u}
	}
	g.proxy = &httputil.ReverseProxy{Director: g.director, ModifyResponse: g.meter}
	return g, nil
}

func (g *Gateway) Mint(vendor string, budgetUSD float64, ttl time.Duration) (string, error) {
	if _, ok := g.vendors[vendor]; !ok {
		return "", fmt.Errorf("gateway: no backend for vendor %q", vendor)
	}
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		// A predictable bearer token would let a co-tenant forge gateway calls.
		// No entropy is unrecoverable; fail closed rather than mint zeros.
		panic("drydock: crypto/rand failed — cannot mint gateway tokens: " + err.Error())
	}
	tok := "tok_" + hex.EncodeToString(b)
	g.mu.Lock()
	g.leases[tok] = &Lease{Token: tok, Vendor: vendor, BudgetUSD: budgetUSD, Expiry: time.Now().Add(ttl)}
	g.mu.Unlock()
	return tok, nil
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
// The bearer is compared against the stored token with subtle.ConstantTimeCompare
// so future changes to the lookup path can't silently introduce a timing
// side-channel on token validation.
func (g *Gateway) check(token string) (*Lease, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.leases[token]
	if l == nil {
		return nil, http.StatusUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(l.Token), []byte(token)) != 1 {
		return nil, http.StatusUnauthorized
	}
	if time.Now().After(l.Expiry) {
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
	vt := g.vendors[lease.Vendor]
	secret, err := vt.cred.Current()
	if err != nil {
		http.Error(w, "credential unavailable", http.StatusBadGateway)
		return
	}
	ctx := context.WithValue(r.Context(), ctxKey{}, &reqCtx{lease: lease, secret: secret})
	g.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (g *Gateway) director(req *http.Request) {
	rc, _ := req.Context().Value(ctxKey{}).(*reqCtx)
	if rc == nil {
		return
	}
	vt, ok := g.vendors[rc.lease.Vendor]
	if !ok {
		return
	}
	req.URL.Scheme = vt.upstream.Scheme
	req.URL.Host = vt.upstream.Host
	req.Host = vt.upstream.Host
	vt.v.Inject(req, rc.secret)
}

// meter tees the response body and, on completion, adds its cost to the lease.
func (g *Gateway) meter(resp *http.Response) error {
	rc, _ := resp.Request.Context().Value(ctxKey{}).(*reqCtx)
	if rc == nil {
		return nil
	}
	vt, ok := g.vendors[rc.lease.Vendor]
	if !ok {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	resp.Body = &usageReader{rc: resp.Body, onDone: func(body []byte) {
		if model, in, out, ok := vt.v.ParseUsage(body, ct); ok {
			g.mu.Lock()
			rc.lease.SpentUSD += cost(vt.v.Prices, model, in, out)
			g.mu.Unlock()
		}
	}}
	return nil
}

// usageMarker is the substring every usage-bearing line carries. Streaming
// agent responses are megabytes of `content_block_delta` events that have no
// usage; only `message_start`/`message_delta` (and the final OpenAI event) do.
var usageMarker = []byte("usage")

// usageReader tees the response body to the client unchanged while metering it,
// without buffering the whole (multi-MB) stream. It scans line by line and
// retains ONLY lines containing "usage" — a handful of small events — then
// hands those to the vendor parser at EOF/Close. Peak memory is one line plus
// the few usage events, not the entire body.
type usageReader struct {
	rc     io.ReadCloser
	line   bytes.Buffer // current incomplete line
	kept   bytes.Buffer // only the usage-bearing lines, preserved verbatim
	onDone func([]byte)
	done   bool
}

func (u *usageReader) Read(p []byte) (int, error) {
	n, err := u.rc.Read(p)
	if n > 0 {
		u.consume(p[:n])
	}
	if err == io.EOF {
		u.finish()
	}
	return n, err
}

func (u *usageReader) consume(b []byte) {
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			u.line.Write(b)
			return
		}
		u.line.Write(b[:i])
		u.flushLine()
		b = b[i+1:]
	}
}

// flushLine keeps the just-completed line iff it carries usage, then resets it.
func (u *usageReader) flushLine() {
	if bytes.Contains(u.line.Bytes(), usageMarker) {
		u.kept.Write(u.line.Bytes())
		u.kept.WriteByte('\n')
	}
	u.line.Reset()
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
	u.flushLine() // a non-streaming body has no trailing newline — flush it
	u.onDone(u.kept.Bytes())
}
