# Per-task Egress Widening via Squid Proxy Auth — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make approved per-task egress widening actually work — an approved extra host becomes reachable by *only* the task it was approved for — by giving host-side squid a per-task identity via proxy auth.

**Architecture:** Squid stays a single shared process. A per-task proxy credential (mirroring the credential gateway's per-task bearer-token model) authorizes a task's *extra* hosts via `proxy_auth` ACLs in per-task `include` fragments. ACL ordering puts the fast `dstdomain` check before the slow `proxy_auth` check so the default (non-widened) path is never challenged. The broker registers/deregisters each widened task's credential + ACL fragment around the container run and triggers `squid -k reconfigure`.

**Tech Stack:** Go 1.26, Homebrew squid 7.6 (`basic_*` auth helpers present), Apple `container` CLI, nftables (in-VM, unchanged).

**Spec:** `docs/superpowers/specs/2026-06-22-egress-widening-squid-proxy-auth-design.md`

## Global Constraints

- Go version floor: `go 1.26` (per `go.mod`). No new third-party dependencies.
- The default (non-widened) egress path must remain observably unchanged: a non-widened task gets a plain `HTTP_PROXY=http://<GW>:3128` (no userinfo), is never sent a 407, and a genuinely-blocked host still returns a deny (403), never a 407.
- The digest-pinned VM image (`image/`) must NOT change — all changes are host-side (squid + broker).
- `auth_param` ACL ordering is load-bearing: fast ACLs (`CONNECT`, `SSL_ports`, `dstdomain`) MUST precede the slow `proxy_auth` ACL in every per-task rule.
- All squid-state mutations (token file, ACL fragments, reconfigure) serialize behind one mutex; failures on the widening path are fail-closed (abort the task before the container runs).
- The squid auth helper is the brokerd binary re-invoked as a hidden subcommand (`__squid-authhelper <tokenfile>`) — no new file shipped to the host.
- Cleanup must ride the existing task `defer` chain so panic/cancel still deregisters.

---

## Task 1: SPIKE — verify the agents honor proxy auth (HARD GATE)

**This task is a go/no-go gate. Everything after it depends on it passing. If it fails, STOP and implement fallback B (per-task squid instance) from the spec instead.**

**Files:** none committed (throwaway manual verification).

**Goal:** Confirm that both `claude-code` and `codex` (as pinned in `image/Dockerfile`) send `Proxy-Authorization` when squid answers a CONNECT with `407 Proxy Authentication Required`, using a proxy URL of the form `http://user:secret@host:port`.

- [ ] **Step 1: Write a throwaway token file and minimal authed squid.conf**

In a scratch dir `/tmp/squidspike`, create `tokens` containing one line:
```
task-spike s3cr3t
```
Create `/tmp/squidspike/squid.conf` (replace `<HELPER>` with a temporary shell helper from Step 2, `<DEFAULTS>` with a file containing `example.com`):
```
http_port 127.0.0.1:3199
auth_param basic program <HELPER> /tmp/squidspike/tokens
auth_param basic children 2
acl authed proxy_auth REQUIRED
acl extra dstdomain api.github.com
acl SSL_ports port 443
acl CONNECT method CONNECT
http_access deny CONNECT !SSL_ports
http_access allow CONNECT SSL_ports extra authed
http_access deny all
cache deny all
access_log none
pid_filename /tmp/squidspike/squid.pid
```

- [ ] **Step 2: Write a temporary shell auth helper**

`/tmp/squidspike/helper.sh` (chmod +x):
```bash
#!/usr/bin/env bash
# squid basic-auth helper: read "user pass" per line, OK if it matches tokens.
while read -r user pass; do
  if grep -qxF "$user $pass" /tmp/squidspike/tokens; then echo OK; else echo ERR; fi
done
```

- [ ] **Step 3: Start squid in the foreground**

Run: `/opt/homebrew/opt/squid/sbin/squid -N -f /tmp/squidspike/squid.conf`
Expected: stays running, no fatal config error.

- [ ] **Step 4: Verify curl (baseline) honors proxy auth**

Run: `https_proxy='http://task-spike:s3cr3t@127.0.0.1:3199' curl -sS -o /dev/null -w '%{http_code}\n' https://api.github.com`
Expected: `200` (or any non-407 from GitHub). A `407` means the proxy URL userinfo path itself is broken — investigate before proceeding.

Run with wrong secret: `https_proxy='http://task-spike:wrong@127.0.0.1:3199' curl -sS -o /dev/null -w '%{http_code}\n' https://api.github.com`
Expected: `407`.

- [ ] **Step 5: Verify the actual agent CLIs honor proxy auth**

Run a claude-code and a codex invocation (matching the pinned versions in `image/Dockerfile`: claude-code `2.1.177`, codex `0.140.0`) inside a throwaway container on the egress network, with `HTTPS_PROXY=http://task-spike:s3cr3t@<GW>:3199` and a prompt that forces a fetch to `api.github.com`. The simplest harness: reuse `cmd/drydock/redteam.go`'s in-VM curl shape (it already runs `init-firewall.sh` + curls a host) but point the proxy env at the authed URL and the spike squid.

Expected: the request reaches `api.github.com` (not blocked at 407). Confirm in squid's stdout that the request authenticated as `task-spike`.

- [ ] **Step 6: Record the verdict**

If both agents authenticate: **GATE PASSED** — proceed to Task 2.
If either agent loops on 407 / cannot send proxy auth: **GATE FAILED** — stop; switch to spec fallback B. Note the failing agent in the spec's Risks section and re-plan.

---

## Task 2: Squid basic-auth helper as a brokerd subcommand

**Files:**
- Create: `cmd/brokerd/authhelper.go`
- Test: `cmd/brokerd/authhelper_test.go`
- Modify: `cmd/brokerd/main.go` (dispatch the hidden subcommand before normal startup)

**Interfaces:**
- Produces: `func runSquidAuthHelper(tokenPath string, in io.Reader, out io.Writer) error` — reads `"<user> <pass>"` lines from `in`, writes `OK`/`ERR` lines to `out`, validating against `tokenPath` (lines of `"<user> <secret>"`). Re-reads the token file each line so the broker can update it live.
- Produces (CLI): `brokerd __squid-authhelper <tokenfile>` invokes the above against os.Stdin/os.Stdout.

- [ ] **Step 1: Write the failing test**

`cmd/brokerd/authhelper_test.go`:
```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSquidAuthHelper_OKAndERR(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "tokens")
	if err := os.WriteFile(tok, []byte("task-a alpha\ntask-b bravo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("task-a alpha\ntask-a wrong\ntask-x alpha\ntask-b bravo\n")
	var out strings.Builder
	if err := runSquidAuthHelper(tok, in, &out); err != nil {
		t.Fatalf("helper returned error: %v", err)
	}
	got := out.String()
	want := "OK\nERR\nERR\nOK\n"
	if got != want {
		t.Errorf("responses = %q, want %q", got, want)
	}
}

func TestRunSquidAuthHelper_MissingTokenFileIsAllERR(t *testing.T) {
	in := strings.NewReader("task-a alpha\n")
	var out strings.Builder
	if err := runSquidAuthHelper(filepath.Join(t.TempDir(), "nope"), in, &out); err != nil {
		t.Fatalf("helper returned error: %v", err)
	}
	if out.String() != "ERR\n" {
		t.Errorf("missing token file should ERR, got %q", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/brokerd/ -run TestRunSquidAuthHelper -v`
Expected: FAIL — `undefined: runSquidAuthHelper`.

- [ ] **Step 3: Write minimal implementation**

`cmd/brokerd/authhelper.go`:
```go
package main

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// runSquidAuthHelper implements squid's basic-auth helper protocol: read one
// "<user> <pass>" line at a time, answer "OK" or "ERR". squid URL-encodes both
// fields. The token file (lines of "<user> <secret>") is re-read per request so
// the broker can add/remove credentials live without restarting the helper.
func runSquidAuthHelper(tokenPath string, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		user, pass := parseHelperLine(sc.Text())
		if user != "" && lookupToken(tokenPath, user) == pass {
			fmt.Fprintln(out, "OK")
		} else {
			fmt.Fprintln(out, "ERR")
		}
	}
	return sc.Err()
}

func parseHelperLine(line string) (user, pass string) {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) < 2 {
		return "", ""
	}
	u, _ := url.QueryUnescape(f[0])
	p, _ := url.QueryUnescape(f[1])
	return u, p
}

// lookupToken returns the secret registered for user, or "" if absent.
func lookupToken(tokenPath, user string) string {
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) == 2 && f[0] == user {
			return f[1]
		}
	}
	return ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/brokerd/ -run TestRunSquidAuthHelper -v`
Expected: PASS (both tests).

- [ ] **Step 5: Wire the hidden subcommand into main**

In `cmd/brokerd/main.go`, at the very top of `main()` (before any normal startup/flag parsing), add:
```go
	// Hidden subcommand: squid invokes this same binary as its basic-auth
	// helper (auth_param basic program <brokerd> __squid-authhelper <tokenfile>).
	if len(os.Args) >= 3 && os.Args[1] == "__squid-authhelper" {
		if err := runSquidAuthHelper(os.Args[2], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "squid-authhelper:", err)
			os.Exit(1)
		}
		return
	}
```
(Confirm `os` and `fmt` are already imported in main.go — they are.)

- [ ] **Step 6: Run the package tests and commit**

Run: `go test ./cmd/brokerd/`
Expected: PASS
```bash
git add cmd/brokerd/authhelper.go cmd/brokerd/authhelper_test.go cmd/brokerd/main.go
git commit -m "brokerd: squid basic-auth helper as __squid-authhelper subcommand"
```

---

## Task 3: New squid base conf (auth_param + default ACL + include)

**Files:**
- Modify: `internal/netfw/squid.go` — replace `CompileSquidConf`
- Test: `internal/netfw/squid_test.go` (add cases; file exists via `netfw_test.go`/`squid_pid_test.go` — create `squid_conf_test.go` if no `squid_test.go`)

**Interfaces:**
- Produces: `func CompileSquidConf(bindAddr, allowlistPath, runDir, helperCmd string) string` — adds `helperCmd` param (the absolute `auth_param basic program` line value, e.g. `"/path/to/brokerd __squid-authhelper /run/task-tokens"`). Renders the default-allow rules, then an `include <runDir>/task-acls/*.conf` line before `http_access deny all`.

> NOTE: this changes `CompileSquidConf`'s signature. Its only caller is `StartSquid` (same package), updated in Task 5. Search confirms no other caller.

- [ ] **Step 1: Write the failing test**

Add to `internal/netfw/squid_conf_test.go`:
```go
package netfw

import (
	"strings"
	"testing"
)

func TestCompileSquidConf_AuthAndInclude(t *testing.T) {
	conf := CompileSquidConf("192.168.66.1:3128", "/run/squid-allow.txt", "/run",
		"/usr/local/bin/brokerd __squid-authhelper /run/task-tokens")

	wantSubstrings := []string{
		"http_port 192.168.66.1:3128",
		`auth_param basic program /usr/local/bin/brokerd __squid-authhelper /run/task-tokens`,
		"acl default_dst dstdomain \"/run/squid-allow.txt\"",
		"http_access allow CONNECT default_dst SSL_ports",
		"http_access allow default_dst",
		"include /run/task-acls/*.conf",
		"http_access deny all",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(conf, s) {
			t.Errorf("conf missing %q\n---\n%s", s, conf)
		}
	}
	// The include line MUST come before the final deny-all (per-task allows
	// are evaluated before the catch-all deny).
	if strings.Index(conf, "include /run/task-acls/*.conf") > strings.LastIndex(conf, "http_access deny all") {
		t.Errorf("include must precede final deny all")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/netfw/ -run TestCompileSquidConf_AuthAndInclude -v`
Expected: FAIL (signature mismatch / missing substrings).

- [ ] **Step 3: Replace CompileSquidConf**

In `internal/netfw/squid.go`, replace the existing `CompileSquidConf` with:
```go
// CompileSquidConf renders a squid.conf that binds bindAddr and allows CONNECT
// to :443 (and plain GET) for hosts in allowlistPath (default dstdomain file),
// with no auth. Per-task widening fragments are pulled in via the trailing
// include; each fragment authorizes a single task's extra hosts behind a
// proxy_auth ACL. helperCmd is the full "program ..." value for the basic-auth
// auth_param (the brokerd binary re-invoked as __squid-authhelper <tokenfile>).
func CompileSquidConf(bindAddr, allowlistPath, runDir, helperCmd string) string {
	return fmt.Sprintf(`http_port %s
auth_param basic program %s
auth_param basic children 2 startup=0 idle=1
acl default_dst dstdomain "%s"
acl SSL_ports port 443
acl CONNECT method CONNECT
http_access deny CONNECT !SSL_ports
http_access allow CONNECT default_dst SSL_ports
http_access allow default_dst
include %s/task-acls/*.conf
http_access deny all
dns_nameservers 1.1.1.1 8.8.8.8
cache deny all
cache_log %s/cache.log
access_log none
pid_filename %s/squid.pid
forwarded_for delete
via off
`, bindAddr, helperCmd, allowlistPath, runDir, runDir, runDir)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/netfw/ -run TestCompileSquidConf_AuthAndInclude -v`
Expected: PASS. (The package won't fully build yet — `StartSquid` still calls the old signature; that's fixed in Task 5. Run the focused test with `-run` only.)

- [ ] **Step 5: Commit**

```bash
git add internal/netfw/squid.go internal/netfw/squid_conf_test.go
git commit -m "netfw: squid base conf gains auth_param + per-task include"
```

---

## Task 4: SquidController — AddTask / RemoveTask / reconfigure

**Files:**
- Create: `internal/netfw/controller.go`
- Test: `internal/netfw/controller_test.go`

**Interfaces:**
- Produces:
```go
type SquidController struct { /* binPath, confPath, runDir, mu */ }
func NewSquidController(binPath, confPath, runDir string) *SquidController
func (c *SquidController) AddTask(user, secret string, domains []string) error
func (c *SquidController) RemoveTask(user string) error
```
- Produces (test seam): `var squidReconfigure = func(binPath, confPath string) error { ... }` — package var so tests capture the call without a live squid.
- AddTask writes/updates the token line for `user`, writes `task-acls/<user>.domains` (one host per line) and `task-acls/<user>.conf` (the fragment), then reconfigures. RemoveTask deletes those three artifacts (token line, `.domains`, `.conf`) and reconfigures. Both serialize on `c.mu`.

- [ ] **Step 1: Write the failing test**

`internal/netfw/controller_test.go`:
```go
package netfw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSquidController_AddRemoveTask(t *testing.T) {
	dir := t.TempDir()
	var reconfigs int
	orig := squidReconfigure
	t.Cleanup(func() { squidReconfigure = orig })
	squidReconfigure = func(_, _ string) error { reconfigs++; return nil }

	c := NewSquidController("/bin/squid", filepath.Join(dir, "squid.conf"), dir)

	if err := c.AddTask("task-abc", "sekret", []string{"api.github.com", "pypi.org"}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	// Token line present.
	tok, _ := os.ReadFile(filepath.Join(dir, "task-tokens"))
	if !strings.Contains(string(tok), "task-abc sekret") {
		t.Errorf("token file missing entry: %q", tok)
	}
	// Domains file: one host per line.
	dom, _ := os.ReadFile(filepath.Join(dir, "task-acls", "task-abc.domains"))
	if string(dom) != "api.github.com\npypi.org\n" {
		t.Errorf("domains = %q", dom)
	}
	// Fragment: fast ACLs (dstdomain) before slow proxy_auth in the rule.
	frag, _ := os.ReadFile(filepath.Join(dir, "task-acls", "task-abc.conf"))
	fs := string(frag)
	for _, want := range []string{
		`acl u_task-abc proxy_auth task-abc`,
		`acl d_task-abc dstdomain "` + filepath.Join(dir, "task-acls", "task-abc.domains") + `"`,
		`http_access allow CONNECT SSL_ports d_task-abc u_task-abc`,
		`http_access allow d_task-abc u_task-abc`,
	} {
		if !strings.Contains(fs, want) {
			t.Errorf("fragment missing %q\n%s", want, fs)
		}
	}
	// d_ (dstdomain) must appear before u_ (proxy_auth) in the CONNECT rule.
	rule := "http_access allow CONNECT SSL_ports d_task-abc u_task-abc"
	if !strings.Contains(fs, rule) {
		t.Errorf("CONNECT rule must order dstdomain before proxy_auth: %s", fs)
	}
	if reconfigs != 1 {
		t.Errorf("AddTask reconfigs = %d, want 1", reconfigs)
	}

	if err := c.RemoveTask("task-abc"); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-acls", "task-abc.conf")); !os.IsNotExist(err) {
		t.Errorf("fragment not removed")
	}
	tok2, _ := os.ReadFile(filepath.Join(dir, "task-tokens"))
	if strings.Contains(string(tok2), "task-abc") {
		t.Errorf("token line not removed: %q", tok2)
	}
	if reconfigs != 2 {
		t.Errorf("RemoveTask reconfigs = %d, want 2", reconfigs)
	}
}

func TestSquidController_RemovePreservesOtherTasks(t *testing.T) {
	dir := t.TempDir()
	orig := squidReconfigure
	t.Cleanup(func() { squidReconfigure = orig })
	squidReconfigure = func(_, _ string) error { return nil }

	c := NewSquidController("/bin/squid", filepath.Join(dir, "squid.conf"), dir)
	_ = c.AddTask("task-1", "s1", []string{"a.com"})
	_ = c.AddTask("task-2", "s2", []string{"b.com"})
	_ = c.RemoveTask("task-1")

	tok, _ := os.ReadFile(filepath.Join(dir, "task-tokens"))
	if strings.Contains(string(tok), "task-1") || !strings.Contains(string(tok), "task-2 s2") {
		t.Errorf("token file after remove = %q", tok)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/netfw/ -run TestSquidController -v`
Expected: FAIL — `undefined: NewSquidController` / `squidReconfigure`.

- [ ] **Step 3: Write the implementation**

`internal/netfw/controller.go`:
```go
package netfw

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// squidReconfigure applies a live config reload. Package var so tests swap it.
var squidReconfigure = func(binPath, confPath string) error {
	out, err := exec.Command(binPath, "-k", "reconfigure", "-f", confPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netfw: squid reconfigure: %w\n%s", err, out)
	}
	return nil
}

// SquidController owns the per-task token file and ACL fragments under runDir
// and serializes all mutations + reconfigures behind a single mutex.
type SquidController struct {
	binPath  string
	confPath string
	runDir   string
	mu       sync.Mutex
}

func NewSquidController(binPath, confPath, runDir string) *SquidController {
	return &SquidController{binPath: binPath, confPath: confPath, runDir: runDir}
}

func (c *SquidController) tokenPath() string  { return filepath.Join(c.runDir, "task-tokens") }
func (c *SquidController) aclDir() string      { return filepath.Join(c.runDir, "task-acls") }
func (c *SquidController) domainsPath(u string) string { return filepath.Join(c.aclDir(), u+".domains") }
func (c *SquidController) fragPath(u string) string    { return filepath.Join(c.aclDir(), u+".conf") }

// AddTask registers user→secret + the user's allowed domains, writes the ACL
// fragment, and reconfigures. ACL ordering: fast dstdomain before slow
// proxy_auth, so non-extra hosts never trigger a 407.
func (c *SquidController) AddTask(user, secret string, domains []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.aclDir(), 0o755); err != nil {
		return err
	}
	if err := c.upsertToken(user, secret); err != nil {
		return err
	}
	if err := os.WriteFile(c.domainsPath(user), []byte(strings.Join(domains, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	frag := fmt.Sprintf(`acl u_%s proxy_auth %s
acl d_%s dstdomain "%s"
http_access allow CONNECT SSL_ports d_%s u_%s
http_access allow d_%s u_%s
`, user, user, user, c.domainsPath(user), user, user, user, user)
	if err := os.WriteFile(c.fragPath(user), []byte(frag), 0o644); err != nil {
		return err
	}
	return squidReconfigure(c.binPath, c.confPath)
}

// RemoveTask deletes the user's token line, domains file, and fragment, then
// reconfigures. Missing artifacts are not an error (idempotent cleanup).
func (c *SquidController) RemoveTask(user string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = os.Remove(c.fragPath(user))
	_ = os.Remove(c.domainsPath(user))
	if err := c.removeToken(user); err != nil {
		return err
	}
	return squidReconfigure(c.binPath, c.confPath)
}

func (c *SquidController) upsertToken(user, secret string) error {
	lines := c.readTokenLines()
	out := lines[:0:0]
	for _, ln := range lines {
		if f := strings.Fields(ln); len(f) >= 1 && f[0] == user {
			continue // drop any prior entry for this user
		}
		out = append(out, ln)
	}
	out = append(out, user+" "+secret)
	return os.WriteFile(c.tokenPath(), []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

func (c *SquidController) removeToken(user string) error {
	lines := c.readTokenLines()
	out := lines[:0:0]
	for _, ln := range lines {
		if f := strings.Fields(ln); len(f) >= 1 && f[0] == user {
			continue
		}
		out = append(out, ln)
	}
	body := ""
	if len(out) > 0 {
		body = strings.Join(out, "\n") + "\n"
	}
	return os.WriteFile(c.tokenPath(), []byte(body), 0o600)
}

func (c *SquidController) readTokenLines() []string {
	b, err := os.ReadFile(c.tokenPath())
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/netfw/ -run TestSquidController -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add internal/netfw/controller.go internal/netfw/controller_test.go
git commit -m "netfw: SquidController add/remove per-task proxy-auth ACL + reconfigure"
```

---

## Task 5: Boot cleanup + StartSquid wiring for the new conf

**Files:**
- Modify: `internal/netfw/squid.go` — `StartSquid` signature (add `helperCmd`), call new `CompileSquidConf`, clear stale `task-acls/` + token file; add `ResetTaskState(runDir)`.
- Test: `internal/netfw/squid_conf_test.go` (add a reset test)

**Interfaces:**
- Produces: `func ResetTaskState(runDir string) error` — removes `runDir/task-acls/` and `runDir/task-tokens` (stale state from a hard-killed prior broker). Called by `StartSquid`.
- Modifies: `func StartSquid(binPath, bindAddr, allowlist, runDir, helperCmd string) (*Squid, error)` — adds trailing `helperCmd` param.

> NOTE: `StartSquid`'s only caller is `cmd/brokerd/main.go:140`, updated in Task 7.

- [ ] **Step 1: Write the failing test**

Add to `internal/netfw/squid_conf_test.go`:
```go
func TestResetTaskState_ClearsStale(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "task-acls"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "task-acls", "task-x.conf"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "task-tokens"), []byte("task-x s\n"), 0o600)

	if err := ResetTaskState(dir); err != nil {
		t.Fatalf("ResetTaskState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-acls")); !os.IsNotExist(err) {
		t.Errorf("task-acls not cleared")
	}
	if _, err := os.Stat(filepath.Join(dir, "task-tokens")); !os.IsNotExist(err) {
		t.Errorf("task-tokens not cleared")
	}
}
```
(Add `"os"` and `"path/filepath"` imports to the test file if not present.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/netfw/ -run TestResetTaskState -v`
Expected: FAIL — `undefined: ResetTaskState`.

- [ ] **Step 3: Implement ResetTaskState and update StartSquid**

In `internal/netfw/squid.go`, add:
```go
// ResetTaskState clears per-task widening artifacts (ACL fragments + token
// file) left by a hard-killed prior broker, so a fresh start begins with only
// the default allowlist. Mirrors reapStaleSquid for the pid file.
func ResetTaskState(runDir string) error {
	if err := os.RemoveAll(filepath.Join(runDir, "task-acls")); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(runDir, "task-tokens")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
```
Then update `StartSquid`'s signature and body. Change the signature line to:
```go
func StartSquid(binPath, bindAddr, allowlist, runDir, helperCmd string) (*Squid, error) {
```
Replace the `CompileSquidConf(...)` call with the new 4-arg form and add the reset before launching squid. The conf-write line becomes:
```go
	if err := os.WriteFile(confPath, []byte(CompileSquidConf(bindAddr, allowPath, runDir, helperCmd)), 0o644); err != nil {
		return nil, err
	}
	// Clear stale per-task widening state from a prior hard-killed broker.
	if err := ResetTaskState(runDir); err != nil {
		return nil, err
	}
```
(Place the `ResetTaskState` call right after writing the conf and before `reapStaleSquid`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/netfw/ -run 'TestResetTaskState|TestCompileSquidConf'`
Expected: PASS. (Full package build still pending the main.go caller — Task 7.)

- [ ] **Step 5: Commit**

```bash
git add internal/netfw/squid.go internal/netfw/squid_conf_test.go
git commit -m "netfw: StartSquid takes helperCmd; clear stale per-task state on boot"
```

---

## Task 6: Broker widened-task lifecycle (extract `setupWidening`, inject authed proxy env)

**Files:**
- Modify: `internal/broker/broker.go` — add `Squid` field + `SquidControl` interface; extract a `setupWidening` method; call it from `HandleTask` and inject the authed proxy URL.
- Test: `internal/broker/widening_test.go` (new file — unit-test `setupWidening` directly)

**Why extract a method:** `HandleTask` is a long handler with no existing unit harness (`broker_test.go:17` notes it's covered only by the host-integration e2e). Rather than build a whole `HandleTask`-driving harness, isolate the widening logic into a small method that is unit-testable on its own with a fake `SquidControl`. `HandleTask` then just calls it.

**Interfaces:**
- Consumes (from Task 4): a value implementing
```go
type SquidControl interface {
	AddTask(user, secret string, domains []string) error
	RemoveTask(user string) error
}
```
- Produces:
  - `Broker.Squid SquidControl` field (nil → widening disabled; non-widened tasks and tests unaffected).
  - `func (b *Broker) setupWidening(taskID string, extras []egress.Domain) (proxyAuth string, cleanup func(), err error)` — when `len(extras)>0 && b.Squid!=nil`, mints a secret, calls `AddTask`, returns `proxyAuth` = `"<user>:<secret>@"` and a `cleanup` that calls `RemoveTask`. Otherwise returns `("", func(){}, nil)`. Always returns a non-nil `cleanup` (safe to `defer cleanup()`).
  - `func proxyUser(taskID string) string { return "task-" + taskID }`
  - `func mintProxySecret() (string, error)` (uses already-imported `crypto/rand` + `encoding/hex`).

- [ ] **Step 1: Write the failing test**

Create `internal/broker/widening_test.go`:
```go
package broker

import (
	"strings"
	"testing"

	"drydock/internal/egress"
)

// fakeSquid records AddTask/RemoveTask calls for lifecycle assertions.
type fakeSquid struct {
	added   []string
	removed []string
	domains map[string][]string
}

func (f *fakeSquid) AddTask(user, secret string, domains []string) error {
	f.added = append(f.added, user)
	if f.domains == nil {
		f.domains = map[string][]string{}
	}
	f.domains[user] = domains
	return nil
}
func (f *fakeSquid) RemoveTask(user string) error {
	f.removed = append(f.removed, user)
	return nil
}

func TestSetupWidening_RegistersAndReturnsAuth(t *testing.T) {
	fs := &fakeSquid{}
	b := &Broker{Squid: fs}
	extras := []egress.Domain{{Host: "api.github.com", Ports: []int{443}}}

	proxyAuth, cleanup, err := b.setupWidening("abc123", extras)
	if err != nil {
		t.Fatalf("setupWidening: %v", err)
	}
	if len(fs.added) != 1 || fs.added[0] != "task-abc123" {
		t.Fatalf("AddTask users = %v, want [task-abc123]", fs.added)
	}
	if got := fs.domains["task-abc123"]; len(got) != 1 || got[0] != "api.github.com" {
		t.Errorf("registered domains = %v", got)
	}
	// proxyAuth must be "user:secret@" with a non-empty secret.
	if !strings.HasPrefix(proxyAuth, "task-abc123:") || !strings.HasSuffix(proxyAuth, "@") || len(proxyAuth) <= len("task-abc123:@") {
		t.Errorf("proxyAuth = %q, want task-abc123:<secret>@", proxyAuth)
	}
	// cleanup deregisters.
	cleanup()
	if len(fs.removed) != 1 || fs.removed[0] != "task-abc123" {
		t.Errorf("cleanup removed = %v, want [task-abc123]", fs.removed)
	}
}

func TestSetupWidening_NoExtrasIsNoOp(t *testing.T) {
	fs := &fakeSquid{}
	b := &Broker{Squid: fs}
	proxyAuth, cleanup, err := b.setupWidening("abc123", nil)
	if err != nil {
		t.Fatal(err)
	}
	if proxyAuth != "" {
		t.Errorf("proxyAuth = %q, want empty for non-widened task", proxyAuth)
	}
	cleanup() // must be safe to call
	if len(fs.added) != 0 || len(fs.removed) != 0 {
		t.Errorf("non-widened touched squid: added=%v removed=%v", fs.added, fs.removed)
	}
}

func TestSetupWidening_NilSquidIsNoOp(t *testing.T) {
	b := &Broker{} // Squid nil
	proxyAuth, cleanup, err := b.setupWidening("abc123", []egress.Domain{{Host: "x.com", Ports: []int{443}}})
	if err != nil {
		t.Fatal(err)
	}
	if proxyAuth != "" {
		t.Errorf("proxyAuth = %q, want empty when Squid is nil", proxyAuth)
	}
	cleanup() // must not panic
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/broker/ -run TestSetupWidening -v`
Expected: FAIL — `b.Squid undefined` / `b.setupWidening undefined`.

- [ ] **Step 3: Add the interface, field, helpers, and method**

In `internal/broker/broker.go`, near the `Broker` struct, add:
```go
// SquidControl registers/deregisters per-task egress widening with squid.
// nil on a Broker disables widening enforcement (non-widened tasks and tests).
type SquidControl interface {
	AddTask(user, secret string, domains []string) error
	RemoveTask(user string) error
}
```
Add a field to `Broker` (next to `ProxyPort`):
```go
	Squid SquidControl // per-task egress widening; nil = disabled
```
Add helpers + method (near `newID`):
```go
func proxyUser(taskID string) string { return "task-" + taskID }

// mintProxySecret returns a random hex secret for a task's proxy credential.
func mintProxySecret() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// setupWidening registers a per-task squid credential + ACL for the task's
// extra hosts and returns the "<user>:<secret>@" userinfo to splice into the
// VM's proxy URL, plus a cleanup that deregisters it. For a non-widened task
// (no extras) or when squid widening is disabled (b.Squid == nil) it is a
// no-op: empty proxyAuth and a no-op cleanup (always safe to defer). Fail-closed:
// a registration error is returned and the caller must abort before the run.
func (b *Broker) setupWidening(taskID string, extras []egress.Domain) (proxyAuth string, cleanup func(), err error) {
	cleanup = func() {}
	if len(extras) == 0 || b.Squid == nil {
		return "", cleanup, nil
	}
	secret, err := mintProxySecret()
	if err != nil {
		return "", cleanup, err
	}
	user := proxyUser(taskID)
	hosts := make([]string, 0, len(extras))
	for _, d := range extras {
		hosts = append(hosts, d.Host)
	}
	if err := b.Squid.AddTask(user, secret, hosts); err != nil {
		return "", cleanup, err
	}
	return user + ":" + secret + "@", func() { _ = b.Squid.RemoveTask(user) }, nil
}
```
`crypto/rand` and `encoding/hex` are already imported in broker.go — no import changes.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/broker/ -run TestSetupWidening -v`
Expected: PASS (all three cases).

- [ ] **Step 5: Call setupWidening from HandleTask and inject the authed proxy URL**

In `HandleTask`, right after the whole widening/approval block (after line 357, before `stageDir := ...`):
```go
	// Register per-task egress widening (no-op for non-widened tasks). The
	// returned userinfo scopes the extra hosts to THIS task's proxy credential;
	// cleanup deregisters on every exit path. Fail-closed.
	proxyAuth, widenCleanup, err := b.setupWidening(taskID, t.EgressExtra)
	if err != nil {
		sw.emit(errorEvent(taskID, "egress widening setup failed", ""))
		return
	}
	defer widenCleanup()
```
Then change the proxy env construction (broker.go:421-422) to splice in `proxyAuth`:
```go
		fmt.Sprintf("HTTPS_PROXY=http://%s%s:%d", proxyAuth, b.GatewayIP, b.ProxyPort),
		fmt.Sprintf("HTTP_PROXY=http://%s%s:%d", proxyAuth, b.GatewayIP, b.ProxyPort),
```
(`proxyAuth` is `""` for non-widened tasks → byte-for-byte identical to today's URL.)

- [ ] **Step 6: Run the full package + commit**

Run: `go test ./internal/broker/`
Expected: PASS
```bash
git add internal/broker/broker.go internal/broker/widening_test.go
git commit -m "broker: setupWidening registers per-task squid cred + authed proxy URL"
```

---

## Task 7: Wire the controller into brokerd startup

**Files:**
- Modify: `cmd/brokerd/main.go` — build the helper command + `SquidController`, pass `helperCmd` to `StartSquid`, set `broker.Squid`.

**Interfaces:**
- Consumes: `netfw.StartSquid(bin, proxyAddr, allowlist, runDir, helperCmd)` (Task 5), `netfw.NewSquidController(bin, confPath, runDir)` (Task 4), `Broker.Squid` (Task 6).

- [ ] **Step 1: Build the helper command and pass it to StartSquid**

In `cmd/brokerd/main.go`, around the squid block (line 131-145), before `StartSquid`:
```go
		self, err := os.Executable()
		if err != nil {
			die("resolve brokerd path for squid auth helper", "err", err)
		}
		tokenPath := filepath.Join(cfg.SquidRunDir, "task-tokens")
		helperCmd := fmt.Sprintf("%s __squid-authhelper %s", self, tokenPath)
```
Change the `StartSquid` call to:
```go
		squid, err = netfw.StartSquid(bin, proxyAddr, netfw.CompileSquidAllowlist(egCfg), cfg.SquidRunDir, helperCmd)
```
(Confirm `path/filepath` and `fmt` are imported — they are.)

- [ ] **Step 2: Construct the controller and set it on the broker**

After squid starts successfully (inside the `else` branch, after `slog.Info("squid listening", ...)`):
```go
		confPath := filepath.Join(cfg.SquidRunDir, "squid.conf")
		squidCtl = netfw.NewSquidController(bin, confPath, cfg.SquidRunDir)
```
Declare `var squidCtl *netfw.SquidController` alongside `var squid *netfw.Squid` (line 135) so it remains nil when squid is unavailable. Then where the `broker.Broker{...}` struct is built (around line 255-263, where `ProxyPort` is set), add:
```go
		Squid: squidCtl, // nil when squid is unavailable → widening disabled
```
(A nil `*netfw.SquidController` stored in the `SquidControl` interface field would be a non-nil interface; to keep the nil-disables semantics, assign through a helper: only set the field when `squidCtl != nil`. Simplest: build the struct, then `if squidCtl != nil { b.Squid = squidCtl }`.)

- [ ] **Step 3: Build and run the whole module**

Run: `go build ./... && go vet ./...`
Expected: clean — all signature changes (CompileSquidConf, StartSquid) now have matching callers.

Run: `go test ./...`
Expected: PASS across all packages.

- [ ] **Step 4: Commit**

```bash
git add cmd/brokerd/main.go
git commit -m "brokerd: construct SquidController, pass auth helper to squid, enable widening"
```

---

## Task 8: Integration assertion — approved reachable, others blocked, isolation holds

**Files:**
- Modify: `cmd/drydock/redteam.go` (or the demo `demo/breach.sh`) — extend the existing egress red-team to assert the *approve* path now works and isolation holds.
- Test: manual/integration run (documented), since this needs real squid + container.

**Goal:** Prove end-to-end that (a) a host approved for task A is reachable from A, (b) a non-approved host still returns deny, and (c) a concurrent task B cannot reach A's approved host.

- [ ] **Step 1: Add the positive + isolation checks to the red-team script**

In the in-VM script section of `cmd/drydock/redteam.go` (the heredoc that runs `init-firewall.sh` then curls hosts), add, for a task whose widening to `api.github.com` was approved:
```bash
# Approved widening must now succeed (was a false 403 before the fix).
code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 https://api.github.com)
echo "approved-host: $code"   # expect 200/301/302, NOT 403/407

# A still-non-approved host must remain blocked (clean deny, not a challenge).
code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 https://example.org || true)
echo "blocked-host: $code"    # expect 403
```

- [ ] **Step 2: Document the cross-task isolation check**

Add a comment block in `redteam.go` describing the manual two-task isolation check: submit task A with approved `api.github.com`; while A runs, submit task B without widening; B's VM curling `https://api.github.com` through the proxy must get a deny (B carries no proxy cred / a different one), proving A's grant does not leak to B.

- [ ] **Step 3: Run the red-team end to end**

Run: `drydock redteam` (or the project's documented red-team entrypoint) against a broker built from this branch, with a task that has approved widening.
Expected: `approved-host: 200` (or a non-4xx redirect), `blocked-host: 403`. Capture the output in the PR description.

- [ ] **Step 4: Commit**

```bash
git add cmd/drydock/redteam.go
git commit -m "redteam: assert approved egress widening reachable + cross-task isolation"
```

---

## Self-Review

**Spec coverage:**
- Chosen approach (proxy auth, ordered ACLs) → Tasks 3, 4.
- Auth helper (custom minimal, brokerd subcommand) → Task 2.
- netfw controller (AddTask/RemoveTask/reconfigure, mutex) → Task 4.
- Boot cleanup of stale state → Task 5.
- Broker lifecycle (mint, register, authed env, deferred deregister, all exit paths) → Task 6 (`setupWidening` + deferred `widenCleanup`).
- main wiring → Task 7.
- Common path unchanged (no userinfo for non-widened) → Task 6 Step 5 + `TestSetupWidening_NoExtrasIsNoOp`/`_NilSquidIsNoOp`.
- Fail-closed on widening error → Task 6 Step 5 (abort before run on `setupWidening` error).
- Concurrency mutex → Task 4.
- Testing (netfw unit, broker lifecycle, integration isolation) → Tasks 3,4,6,8.
- Spike gate → Task 1.

**Placeholder scan:** No "TBD/TODO/handle errors". The two adaptation notes (Task 6 harness names; Task 8 integration entrypoint) are explicit "inspect existing X, reuse it" instructions, not deferred code — the surrounding code is complete.

**Type consistency:** `AddTask(user, secret string, domains []string)` / `RemoveTask(user string)` identical across Tasks 4 and 6. `CompileSquidConf(bindAddr, allowlistPath, runDir, helperCmd string)` consistent Tasks 3,5,7. `StartSquid(..., helperCmd string)` consistent Tasks 5,7. `setupWidening(taskID string, extras []egress.Domain) (string, func(), error)`, `proxyUser`, `mintProxySecret` (hex, no new import) defined and used in Task 6. `squidReconfigure(binPath, confPath string)` consistent Task 4. `runSquidAuthHelper(tokenPath string, in io.Reader, out io.Writer) error` consistent Task 2.

**Verified against the codebase:** `cmd/brokerd/main.go` already imports `os`/`fmt`/`path/filepath`; `internal/broker/broker.go` already imports `crypto/rand`/`encoding/hex` (Task 6 uses hex, adds no imports); `cfg.SquidRunDir` exists (`internal/config/config.go:60`). Task 6 was restructured to unit-test an extracted `setupWidening` method (there is NO existing `HandleTask` unit harness — `broker_test.go:17` notes it's covered by host-integration e2e only), so it references no invented helpers.

**Known adaptation point (not a placeholder):** Task 8's red-team entrypoint must be matched to the existing `cmd/drydock/redteam.go` in-VM script shape; the plan instructs extending it, not inventing a harness.
