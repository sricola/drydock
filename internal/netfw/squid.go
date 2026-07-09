package netfw

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// CompileSquidConf renders a squid.conf that binds bindAddr and enforces the
// default egress policy from defaultACLPath (a generated block of per-domain
// dstdomain+port ACLs and their allow rules — see CompileSquidAllowlist), with
// no auth. That include sits after the SSL_ports/CONNECT ACLs it references and
// after the global `deny CONNECT !SSL_ports` guard (CONNECT stays 443-only).
// Per-task widening fragments are pulled in via the trailing include; each
// fragment authorizes a single task's extra hosts (with their own port ACLs)
// behind a proxy_auth ACL. helperCmd is the full "program ..." value for the
// basic-auth auth_param (the brokerd binary re-invoked as __squid-authhelper
// <tokenfile>).
func CompileSquidConf(bindAddr, defaultACLPath, runDir, helperCmd string) string {
	return fmt.Sprintf(`http_port %s
auth_param basic program %s
auth_param basic children 2 startup=0 idle=1
acl SSL_ports port 443
acl CONNECT method CONNECT
http_access deny CONNECT !SSL_ports
# SSRF guard: squid resolves allowlist hostnames itself, on the host, outside
# the VM's nft pin. Deny any destination resolving to a private, loopback,
# link-local (incl. cloud metadata 169.254.169.254), or CGNAT address BEFORE
# the allowlist, so an allowlisted/widened name pointed at an internal IP (or
# DNS rebinding) can't reach host-local services or the operator's LAN.
acl to_local dst 127.0.0.0/8 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16 100.64.0.0/10 ::1 fc00::/7 fe80::/10
http_access deny to_local
include %s
include %s/task-acls/*.conf
http_access deny all
dns_nameservers 1.1.1.1 8.8.8.8
cache deny all
cache_log %s/cache.log
access_log %s/access.log squid
logfile_rotate 10
pid_filename %s/squid.pid
forwarded_for delete
via off
`, bindAddr, helperCmd, defaultACLPath, runDir, runDir, runDir, runDir)
}

// taskACLPlaceholder is a comment-only fragment kept in task-acls/ so squid's
// `include .../task-acls/*.conf` always matches at least one file. squid FATALs
// on a glob that matches zero files, which is the normal state (no widened task
// active, and always at boot) — so the placeholder must always be present.
const taskACLPlaceholder = "00-placeholder.conf"

// ResetTaskState clears per-task widening artifacts (ACL fragments + token
// file) left by a hard-killed prior broker, so a fresh start begins with only
// the default allowlist. It leaves task-acls/ present with the comment-only
// placeholder so squid's include resolves with zero active tasks. Mirrors
// reapStaleSquid for the pid file.
func ResetTaskState(runDir string) error {
	aclDir := filepath.Join(runDir, "task-acls")
	if err := os.RemoveAll(aclDir); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(runDir, "task-tokens")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(aclDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(aclDir, taskACLPlaceholder),
		[]byte("# placeholder so squid's task-acls/*.conf include always matches a file\n"), 0o644)
}

// Squid is a handle to a running userspace squid process.
type Squid struct {
	cmd    *exec.Cmd
	runDir string
}

// FindSquid locates the squid binary (Homebrew layout, Debian/Ubuntu sbin, or PATH).
func FindSquid() (string, error) {
	for _, c := range []string{
		"/opt/homebrew/opt/squid/sbin/squid",
		"/opt/homebrew/sbin/squid",
		"/usr/local/sbin/squid",
		"/usr/sbin/squid", // Debian/Ubuntu (apt install squid) — used by the CI egress job
	} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	if p, err := exec.LookPath("squid"); err == nil {
		return p, nil
	}
	return "", errors.New("netfw: squid binary not found (brew install squid)")
}

// StartSquid writes the generated default-ACL block + conf into runDir and
// launches squid in the foreground (-N) bound to bindAddr (e.g.
// 192.168.66.1:3128). defaultACL is the CompileSquidAllowlist output — a squid
// config fragment of per-domain dstdomain+port ACLs and allow rules — which the
// conf pulls in via `include`.
func StartSquid(binPath, bindAddr, defaultACL, runDir, helperCmd string) (*Squid, error) {
	// squid.conf interpolates runDir into unquoted paths (the include glob,
	// pid_filename, cache_log) and the auth helper command, none of which squid
	// parses with embedded whitespace — and the include glob can't be quoted
	// without ceasing to be a glob. Fail fast with a clear message instead of
	// emitting a squid.conf that FATALs at boot or silently breaks proxy auth.
	if strings.ContainsAny(runDir, " \t") {
		return nil, fmt.Errorf("netfw: squid run dir must not contain whitespace: %q", runDir)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	defaultACLPath := filepath.Join(runDir, "squid-default-acl.conf")
	confPath := filepath.Join(runDir, "squid.conf")
	if err := os.WriteFile(defaultACLPath, []byte(defaultACL), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(confPath, []byte(CompileSquidConf(bindAddr, defaultACLPath, runDir, helperCmd)), 0o644); err != nil {
		return nil, err
	}
	// Clear stale per-task widening state from a prior hard-killed broker.
	if err := ResetTaskState(runDir); err != nil {
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
