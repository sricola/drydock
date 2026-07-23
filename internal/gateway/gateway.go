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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Lease struct {
	// Token is the value Mint returned. Stored so check() can do a
	// constant-time equality against the bearer the caller presented;
	// even if Go's map lookup ever changes to a timing-sensitive shape,
	// the defense-in-depth comparison stops timing-side-channel leakage.
	Token             string
	Vendor            string
	BudgetUSD         float64
	SpentUSD          float64
	Expiry            time.Time
	MaxRequests       int     // 0 = unlimited
	Requests          int     // number of requests served so far
	MaxRequestCostUSD float64 // per-request reservation R (0 = disabled)
	Reserved          float64 // sum of R for admitted-but-unmetered requests; guarded by g.mu
	MaxInFlight       int     // max concurrently admitted requests (0 = unlimited)
	InFlight          int     // admitted, response not yet complete; guarded by g.mu
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

	// Aggregate budget cap (0 aggBudget = disabled). Set once at boot via
	// SetAggregateCap, before any request is served.
	ledger     *spendLedger
	aggBudget  float64
	aggVendors map[string]bool
}

type ctxKey struct{}

func New(backends ...Backend) (*Gateway, error) {
	g := &Gateway{leases: map[string]*Lease{}, vendors: map[string]vendorRT{},
		ledger: newSpendLedger(0), aggVendors: map[string]bool{}}
	for _, b := range backends {
		if b.Cred == nil {
			return nil, fmt.Errorf("gateway: backend %q has nil Cred", b.Vendor.Name)
		}
		u, err := url.Parse(b.Vendor.BaseURL)
		if err != nil {
			return nil, err
		}
		g.vendors[b.Vendor.Name] = vendorRT{v: b.Vendor, cred: b.Cred, upstream: u}
	}
	g.proxy = &httputil.ReverseProxy{Director: g.director, ModifyResponse: g.meter}
	return g, nil
}

func (g *Gateway) Mint(vendor string, budgetUSD float64, maxRequests int, maxRequestCostUSD float64, maxInFlight int, ttl time.Duration) (string, error) {
	if _, ok := g.vendors[vendor]; !ok {
		return "", fmt.Errorf("gateway: no backend for vendor %q", vendor)
	}
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		// A predictable bearer token would let a co-tenant forge gateway calls.
		// No entropy is unrecoverable; fail closed rather than mint zeros.
		panic("drydock: crypto/rand failed - cannot mint gateway tokens: " + err.Error())
	}
	tok := "tok_" + hex.EncodeToString(b)
	g.mu.Lock()
	g.leases[tok] = &Lease{Token: tok, Vendor: vendor, BudgetUSD: budgetUSD,
		MaxRequests: maxRequests, MaxRequestCostUSD: maxRequestCostUSD,
		MaxInFlight: maxInFlight, Expiry: time.Now().Add(ttl)}
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

// SetAggregateCap enables the per-vendor aggregate USD cap. Call once at boot
// before serving. vendors is the set the cap applies to (api_key-mode only).
//
// These fields (aggBudget, aggVendors, ledger) are read on the request path
// without g.mu (meter, AggregateExceeded) and under g.mu (admit). The writes
// here are unsynchronized, so the caller MUST finish this call before starting
// the goroutine that serves requests: publish the gateway to that goroutine
// only afterward. brokerd does this (SetAggregateCap precedes the serve loop).
func (g *Gateway) SetAggregateCap(budgetUSD float64, window time.Duration, vendors []string) {
	g.aggBudget = budgetUSD
	g.aggVendors = map[string]bool{}
	for _, v := range vendors {
		g.aggVendors[v] = true
	}
	g.ledger = newSpendLedger(window)
}

// SeedAggregate adds historical spend to the ledger (boot seed from the audit
// trail). No-op when the cap is disabled.
func (g *Gateway) SeedAggregate(vendor string, usd float64, ts time.Time) {
	if g.aggBudget <= 0 {
		return
	}
	g.ledger.add(vendor, usd, ts)
}

// AggregateExceeded reports whether vendor is at or over its aggregate cap. Used
// by the broker for a submit-time pre-check. False when the cap is disabled or
// vendor is out of scope (e.g. subscription).
func (g *Gateway) AggregateExceeded(vendor string) bool {
	return g.aggBudget > 0 && g.aggVendors[vendor] &&
		g.ledger.windowed(vendor, time.Now()) >= g.aggBudget
}

// admit returns (lease, 0, false) when usable, or (nil, statusCode, retryable)
// to reject. retryable is true only for the transient in-flight-limit 429
// (the in-flight slot frees on its own as the concurrent request completes);
// every other rejection, including the terminal per-lease MaxRequests
// exhaustion, is not retryable — no future admit call for this lease can ever
// succeed differently. The bearer is compared against the stored token with
// subtle.ConstantTimeCompare so future changes to the lookup path can't
// silently introduce a timing side-channel on token validation. Named admit
// (not check) to reflect that it mutates l.Requests++ on success — it's an
// authoritative admission decision.
func (g *Gateway) admit(token string) (*Lease, int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.leases[token]
	if l == nil {
		return nil, http.StatusUnauthorized, false
	}
	if subtle.ConstantTimeCompare([]byte(l.Token), []byte(token)) != 1 {
		return nil, http.StatusUnauthorized, false
	}
	if time.Now().After(l.Expiry) {
		return nil, http.StatusUnauthorized, false
	}
	if l.SpentUSD >= l.BudgetUSD {
		return nil, http.StatusPaymentRequired, false
	}
	if l.MaxRequestCostUSD > 0 &&
		l.SpentUSD+l.Reserved+l.MaxRequestCostUSD > l.BudgetUSD {
		return nil, http.StatusPaymentRequired, false
	}
	if l.MaxRequests > 0 && l.Requests >= l.MaxRequests {
		// Terminal: this lease will never admit another request, so a
		// Retry-After would only invite the agent to spin on a condition
		// that never clears.
		return nil, http.StatusTooManyRequests, false
	}
	if l.MaxInFlight > 0 && l.InFlight >= l.MaxInFlight {
		// Spend is metered when a response body completes, so every
		// concurrently admitted request can overshoot the budget by its own
		// cost. Serialize instead of admitting the race (F-02/F-05). This is
		// the one retryable case: the in-flight slot frees as soon as the
		// concurrent request completes.
		return nil, http.StatusTooManyRequests, true
	}
	if g.aggBudget > 0 && g.aggVendors[l.Vendor] &&
		g.ledger.windowed(l.Vendor, time.Now()) >= g.aggBudget {
		return nil, http.StatusPaymentRequired, false
	}
	l.Requests++
	l.InFlight++
	if l.MaxRequestCostUSD > 0 {
		l.Reserved += l.MaxRequestCostUSD
	}
	return l, 0, false
}

// release marks one admitted request complete, freeing its in-flight slot.
// Looked up by token: the lease may have been revoked mid-request.
func (g *Gateway) release(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := g.leases[token]; l != nil && l.InFlight > 0 {
		l.InFlight--
	}
}

// leaseVendor returns the vendor bound to token without mutating the lease, so
// the route allowlist can be selected before admit runs its budget/counter
// admission. ok is false for an unknown token (admit then returns 401).
func (g *Gateway) leaseVendor(token string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := g.leases[token]; l != nil {
		return l.Vendor, true
	}
	return "", false
}

// pathHasTraversal reports whether p contains a ".." path segment. A legitimate
// inference route never does; rejecting it stops a prefix-allowlisted path like
// /v1/messages/../files (or /responses/../admin under a BasePath join) from
// resolving to a control-plane route upstream.
func pathHasTraversal(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := ""
	auth := r.Header.Get("Authorization")
	switch {
	case len(auth) > 7 && auth[:7] == "Bearer ":
		tok = auth[7:]
	case auth == "":
		// Only when there is NO Authorization header do we accept the per-task
		// bearer from x-goog-api-key — the Gemini CLI (API-key mode) presents it
		// there. A present-but-malformed Authorization is not a fallback path, so
		// a stray Google key header can't bypass a bad bearer.
		tok = r.Header.Get("X-Goog-Api-Key")
	}
	// Enforce the per-vendor route allowlist and reject path traversal BEFORE
	// budget admission, so a disallowed or traversing route never reaches the
	// upstream with the real credential (and never consumes a request slot). The
	// vendor lookup is read-only; a bad token falls through to admit's 401.
	if vendor, ok := g.leaseVendor(tok); ok {
		if pathHasTraversal(r.URL.Path) {
			http.Error(w, "forbidden path", http.StatusForbidden)
			return
		}
		if vt, ok := g.vendors[vendor]; ok && !vt.v.routeAllowed(r.Method, r.URL.Path) {
			http.Error(w, "forbidden route", http.StatusForbidden)
			return
		}
	}
	lease, status, retryable := g.admit(tok)
	if status != 0 {
		if retryable {
			// A sequential agent CLI only hits this when a side call overlaps
			// its main stream; SDKs back off and retry on 429. Set only for
			// the transient in-flight case: the terminal per-lease
			// MaxRequests exhaustion never clears, so it must not invite a
			// retry loop.
			w.Header().Set("Retry-After", "1")
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	// ReverseProxy.ServeHTTP streams the response body before returning, so
	// metering (which runs at body EOF) has already settled SpentUSD when the
	// slot frees: the next admitted request sees the updated spend.
	defer g.release(tok)
	// Bound the request body: stripRequestFields (subscription) buffers it with
	// io.ReadAll, and the proxy streams it otherwise. A legit LLM request is well
	// under this; the cap stops a task from OOMing the gateway with a giant body.
	r.Body = http.MaxBytesReader(w, r.Body, maxProxyRequestBytes)
	vt := g.vendors[lease.Vendor]
	secret, err := vt.cred.Current()
	if err != nil {
		http.Error(w, "credential unavailable", http.StatusBadGateway)
		return
	}
	if len(vt.v.StripFields) > 0 {
		stripRequestFields(r, vt.v.StripFields)
	}
	ctx := context.WithValue(r.Context(), ctxKey{}, &reqCtx{lease: lease, secret: secret})
	g.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// maxProxyRequestBytes caps a forwarded request body. A var only so tests can
// lower it; nothing in production writes it.
var maxProxyRequestBytes int64 = 16 << 20 // 16 MiB

// stripRequestFields rewrites a JSON request body to remove top-level fields
// the upstream rejects (see Vendor.StripFields). It buffers the request body
// (small — a messages request, not the streamed response), so it runs only for
// vendors that declare StripFields. No-op unless the body is a JSON object and
// a listed field is present.
func stripRequestFields(r *http.Request, fields []string) {
	if r.Body == nil || !strings.Contains(r.Header.Get("Content-Type"), "json") {
		return
	}
	raw, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		// Restore whatever bytes were read before the error so downstream
		// sees a readable body rather than a closed/empty one.
		r.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}
	body := raw
	if out, changed := stripJSONObjectFields(raw, fields); changed {
		body = out
	}
	// GetBody is intentionally left unset: the reverse proxy doesn't replay this
	// request, so redirect/retry body-replay is not supported here.
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

// stripJSONObjectFields removes the named top-level keys from a JSON object,
// preserving every other key's value verbatim (json.RawMessage). Returns
// (raw, false) when the body is not a JSON object or no named field was present.
func stripJSONObjectFields(raw []byte, fields []string) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, false
	}
	changed := false
	for _, f := range fields {
		if _, ok := m[f]; ok {
			delete(m, f)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw, false
	}
	return out, true
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
	if vt.v.BasePath != "" {
		// The VM's Codex posts to {gateway}/responses (api-key mode); the Codex
		// subscription backend serves it under /backend-api/codex. Tolerate an
		// optional /v1 prefix so either OPENAI_BASE_URL form maps correctly.
		req.URL.Path = singleJoiningSlash(vt.v.BasePath, strings.TrimPrefix(req.URL.Path, "/v1"))
	}
	vt.v.Inject(req, rc.secret)
}

func singleJoiningSlash(a, b string) string {
	aslash, bslash := strings.HasSuffix(a, "/"), strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		if a == "" {
			return b
		}
		return a + "/" + b
	}
	return a + b
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
		delta := 0.0
		if model, in, out, ok := vt.v.ParseUsage(body, ct); ok {
			delta = cost(vt.v.Prices, model, in, out)
		}
		g.mu.Lock()
		if rc.lease.MaxRequestCostUSD > 0 {
			rc.lease.Reserved -= rc.lease.MaxRequestCostUSD
			if rc.lease.Reserved < 0 {
				rc.lease.Reserved = 0 // floor: defense in depth
			}
		}
		rc.lease.SpentUSD += delta
		g.mu.Unlock()
		if g.aggBudget > 0 && delta > 0 {
			g.ledger.add(rc.lease.Vendor, delta, time.Now())
		}
	}}
	return nil
}

// usageMarker is the substring every usage-bearing line carries. Streaming
// agent responses are megabytes of `content_block_delta` events that have no
// usage; only `message_start`/`message_delta` (and the final OpenAI event) do.
var usageMarker = []byte("usage")

// maxUsageBufBytes caps each metering buffer (the current line and the retained
// usage lines) so a pathological response, one giant newline-free body, or an
// endless stream of "usage"-containing lines, cannot grow gateway memory
// unbounded. Metering is a safety gate: under-metering a pathological response
// (truncated JSON parses to no usage) is acceptable. A var only for tests.
var maxUsageBufBytes = 1 << 20 // 1 MiB per buffer

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
			u.appendLine(b)
			return
		}
		u.appendLine(b[:i])
		u.flushLine()
		b = b[i+1:]
	}
}

// appendLine writes b into the current line, bounded by maxUsageBufBytes so a
// giant newline-free body cannot grow u.line without limit. Excess is dropped
// (a line that long is not a parseable usage event).
func (u *usageReader) appendLine(b []byte) {
	if room := maxUsageBufBytes - u.line.Len(); room > 0 {
		if len(b) > room {
			b = b[:room]
		}
		u.line.Write(b)
	}
}

// flushLine keeps the just-completed line iff it carries usage, then resets it.
// Retained usage lines are bounded by maxUsageBufBytes so an endless stream of
// "usage"-containing lines cannot grow u.kept without limit.
func (u *usageReader) flushLine() {
	if u.kept.Len() < maxUsageBufBytes && bytes.Contains(u.line.Bytes(), usageMarker) {
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
