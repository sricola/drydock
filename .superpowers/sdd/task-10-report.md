# Task 10 Report: Shared unix-socket broker client + merge duplicate domain struct

## Constructor signature

```go
// internal/brokerclient/brokerclient.go
func New(dialFn func() (net.Conn, error), timeout time.Duration) (*http.Client, string)
func ResolveAddr() string
func ResolveSocketPath() string
```

`New` returns `(*http.Client, baseURL)`. If `dialFn != nil`, builds a unix-socket client using the injected dialer (base = `"http://brokerd"`). If `dialFn == nil`, resolves BROKER_ADDR env → config.yaml `broker.addr` → unix socket (BROKER_SOCKET env → config.yaml `broker.socket` → `sockpath.Default()`). `timeout = 0` → no client timeout.

## Call site migrations

### cmd/drydock/client.go
- Imports replaced: removed `context`, `net`, `drydock/internal/config`, `drydock/internal/sockpath`; added `drydock/internal/brokerclient`.
- `socketPath()` kept as thin shim: `return brokerclient.ResolveSocketPath()` (still needed by `brokerdDown`).
- `brokerClient()` body replaced with `return brokerclient.New(nil, 5*time.Second)`.

### cmd/drydock/submit.go
- Imports replaced: removed `net`; added `drydock/internal/brokerclient`.
- `reqDomain` struct deleted; `taskRequest.EgressExtra` and `parseEgressExtras` updated to use `domain` (from client.go, same package).
- `submit_test.go` updated: `reqDomain` references replaced with `domain`.
- No-timeout path: `brokerclient.New(nil, 0)` — passes `timeout=0` which leaves `client.Timeout` at zero, preserving the long-running-stream behavior exactly.

### cmd/drydock/ui.go
- Added import `drydock/internal/brokerclient`.
- `BrokerDial` injection changed from `net.Dial("unix", socketPath())` to `net.Dial("unix", brokerclient.ResolveSocketPath())`.

### internal/webui/server.go + submit.go
- `Server` struct gains three unexported fields: `broker *http.Client`, `brokerBase string`, `brokerNoTimeout *http.Client`.
- `Handler()` initialises both clients once (if `BrokerDial != nil`) via `brokerclient.New(s.BrokerDial, 5*time.Second)` and `brokerclient.New(s.BrokerDial, 0)`.
- `brokerClient()` method removed entirely.
- `proxy()` now uses `s.broker` / `s.brokerBase` directly, nil-checks `s.broker` (same guard as the former `BrokerDial == nil` check).
- `handleSubmit()` (submit.go): local `http.Client` + `http.Transport` construction removed; uses `s.brokerNoTimeout` and `s.brokerBase`. Still uses `context.Background()` for the request — task lifetime is independent of the HTTP handler.

## No-timeout submit path preservation

`brokerclient.New(nil, 0)` and `brokerclient.New(s.BrokerDial, 0)` both leave `c.Timeout = 0` (the zero-value). The `newUnix` helper's `if timeout > 0` guard ensures no timeout is ever set when the caller passes 0. Verified by `TestNewZeroTimeout`.

## Per-request transport elimination

`grep -n "http.Transport" internal/webui/server.go internal/webui/submit.go` returns no results. A single `*http.Transport` is embedded in the `http.Client` built once in `Handler()`. Previously, `brokerClient()` built a fresh `http.Transport` on every proxied request (every poll cycle at 0.5–1.5s intervals). The fix allows HTTP/1.1 keep-alive connection reuse to brokerd.

## Test results

`go test -race ./...` — all packages green, 0 failures, 0 race conditions.

New tests in `internal/brokerclient/brokerclient_test.go`:
- `TestNewInjectedDialFn` — injected dialFn overrides env; makes a real unix-socket round trip.
- `TestNewBROKER_ADDR` — BROKER_ADDR env selects TCP path, base URL matches env value, Transport is nil (default).
- `TestNewUnixFallback` — no env → unix path with custom Transport.
- `TestNewZeroTimeout` — timeout=0 leaves Timeout=0 on both branches.
- `TestResolveAddr` — env and empty cases.
- `TestResolveSocketPath` — BROKER_SOCKET override.
