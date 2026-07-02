package netfw

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"drydock/internal/egress"
)

// squidReconfigure applies a live config reload. Package var so tests swap it.
var squidReconfigure = func(binPath, confPath string) error {
	out, err := exec.Command(binPath, "-k", "reconfigure", "-f", confPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netfw: squid reconfigure: %w\n%s", err, out)
	}
	return nil
}

// squidRotate rolls the access/cache logs, keeping the `logfile_rotate`
// generations set in the conf. Package var so tests swap it.
var squidRotate = func(binPath, confPath string) error {
	out, err := exec.Command(binPath, "-k", "rotate", "-f", confPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netfw: squid rotate: %w\n%s", err, out)
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

func (c *SquidController) tokenPath() string { return filepath.Join(c.runDir, "task-tokens") }
func (c *SquidController) aclDir() string    { return filepath.Join(c.runDir, "task-acls") }
func (c *SquidController) domainsPath(u string) string {
	return filepath.Join(c.aclDir(), u+".domains")
}
func (c *SquidController) fragPath(u string) string { return filepath.Join(c.aclDir(), u+".conf") }

// AddTask registers user→secret + the user's widened domains, writes the ACL
// fragment, and reconfigures. Each domain gets a dstdomain + port ACL pair so
// squid enforces the per-domain ports (not just the hostname). ACL ordering
// within each allow rule: fast dstdomain/port before the slow proxy_auth ACL,
// so a request for a host/port this task never widened short-circuits before
// squid ever prompts for auth (no spurious 407).
func (c *SquidController) AddTask(user, secret string, domains []egress.Domain) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(c.aclDir(), 0o755); err != nil {
		return err
	}
	if err := c.upsertToken(user, secret); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "acl u_%s proxy_auth %s\n", user, user)
	for i, d := range domains {
		writeDomainACL(&b,
			fmt.Sprintf("d_%s_%d", user, i),
			fmt.Sprintf("p_%s_%d", user, i),
			d, "u_"+user)
	}
	// 0o600: per-task squid config read only by the broker-owned squid; no other
	// local user needs it.
	if err := os.WriteFile(c.fragPath(user), []byte(b.String()), 0o600); err != nil {
		return err
	}
	return squidReconfigure(c.binPath, c.confPath)
}

// RemoveTask deletes the user's token line, domains file, and fragment, then
// reconfigures. A missing artifact is not an error (idempotent cleanup), but any
// other removal failure (e.g. EPERM) is surfaced rather than silently leaving a
// stale fragment that a later reconfigure would reload.
func (c *SquidController) RemoveTask(user string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Remove(c.fragPath(user)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(c.domainsPath(user)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := c.removeToken(user); err != nil {
		return err
	}
	return squidReconfigure(c.binPath, c.confPath)
}

// Rotate rolls squid's access/cache logs so they don't grow without bound on a
// long-running broker. It keeps the `logfile_rotate` generations from the conf.
// Serialized behind the same mutex as reconfigures so it can't race a widening.
func (c *SquidController) Rotate() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return squidRotate(c.binPath, c.confPath)
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
