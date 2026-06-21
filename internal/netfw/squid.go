package netfw

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// CompileSquidConf renders a squid.conf that binds bindAddr and allows CONNECT
// to :443 (and plain GET) only for hosts in allowlistPath (a dstdomain file).
// No TLS interception; squid resolves names host-side. Logs/pid go under runDir
// so squid needs no privileged default paths.
func CompileSquidConf(bindAddr, allowlistPath, runDir string) string {
	return fmt.Sprintf(`http_port %s
acl allowed dstdomain "%s"
acl SSL_ports port 443
acl CONNECT method CONNECT
http_access deny CONNECT !SSL_ports
http_access deny CONNECT !allowed
http_access allow CONNECT allowed SSL_ports
http_access allow allowed
http_access deny all
dns_nameservers 1.1.1.1 8.8.8.8
cache deny all
cache_log %s/cache.log
access_log none
pid_filename %s/squid.pid
forwarded_for delete
via off
`, bindAddr, allowlistPath, runDir, runDir)
}

// Squid is a handle to a running userspace squid process.
type Squid struct {
	cmd    *exec.Cmd
	runDir string
}

// FindSquid locates the squid binary (Homebrew layout or PATH).
func FindSquid() (string, error) {
	for _, c := range []string{
		"/opt/homebrew/opt/squid/sbin/squid",
		"/opt/homebrew/sbin/squid",
		"/usr/local/sbin/squid",
	} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	if p, err := exec.LookPath("squid"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("netfw: squid binary not found (brew install squid)")
}

// StartSquid writes the allowlist + conf into runDir and launches squid in the
// foreground (-N) bound to bindAddr (e.g. 192.168.66.1:3128).
func StartSquid(binPath, bindAddr, allowlist, runDir string) (*Squid, error) {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	allowPath := filepath.Join(runDir, "squid-allow.txt")
	confPath := filepath.Join(runDir, "squid.conf")
	if err := os.WriteFile(allowPath, []byte(allowlist), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(confPath, []byte(CompileSquidConf(bindAddr, allowPath, runDir)), 0o644); err != nil {
		return nil, err
	}
	// A broker that was hard-killed (SIGKILL, crash) leaves squid's pid file
	// behind; squid then refuses to start ("already running"). Clear a stale
	// one first so a restart self-heals.
	if err := reapStaleSquid(runDir); err != nil {
		return nil, err
	}
	cmd := exec.Command(binPath, "-N", "-f", confPath)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("netfw: start squid: %w", err)
	}
	return &Squid{cmd: cmd, runDir: runDir}, nil
}

// Stop terminates the squid process and reaps it.
func (s *Squid) Stop() error {
	if s == nil {
		return nil
	}
	// Remove the pid file even when there's no live process, so a hard-killed
	// squid (which can't clean up after itself) doesn't block the next start.
	if s.runDir != "" {
		_ = os.Remove(filepath.Join(s.runDir, "squid.pid"))
	}
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	if err := s.cmd.Process.Kill(); err != nil {
		return err
	}
	// Reap so squid doesn't linger as a zombie until brokerd exits. The wait
	// error is just the "signal: killed" status; ignore it.
	_ = s.cmd.Wait()
	return nil
}

// reapStaleSquid clears a leftover squid.pid in runDir. The pid file is removed
// when its PID is dead, unparseable, or belongs to some unrelated live process
// (PID reuse) — i.e. whenever it isn't actually a running squid. If a real squid
// is still bound to this run dir, it returns an error rather than killing it,
// since that usually means another broker is already running.
func reapStaleSquid(runDir string) error {
	pidPath := filepath.Join(runDir, "squid.pid")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return nil // no pid file (or unreadable) — nothing to reap
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err == nil && pid > 0 && processAlive(pid) && processIsSquid(pid) {
		return fmt.Errorf("netfw: a squid is already running (pid %d) for %s — "+
			"another drydock broker may be active; stop it first (or `kill %d`)", pid, runDir, pid)
	}
	return os.Remove(pidPath)
}

// processAlive reports whether pid names a live process. signal 0 performs the
// existence/permission check without delivering a signal.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM // EPERM = alive but not ours to signal
}

// processIsSquid reports whether pid's executable is squid, guarding against
// PID reuse before we'd ever treat a live process as a real squid.
func processIsSquid(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "squid")
}
