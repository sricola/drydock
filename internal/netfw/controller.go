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

func (c *SquidController) tokenPath() string { return filepath.Join(c.runDir, "task-tokens") }
func (c *SquidController) aclDir() string    { return filepath.Join(c.runDir, "task-acls") }
func (c *SquidController) domainsPath(u string) string {
	return filepath.Join(c.aclDir(), u+".domains")
}
func (c *SquidController) fragPath(u string) string { return filepath.Join(c.aclDir(), u+".conf") }

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
