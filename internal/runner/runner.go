// Package runner builds the `container` CLI argv for a sandbox task.
package runner

import (
	"fmt"
)

type Spec struct {
	TaskID     string
	Network    string
	ImageRef   string
	Env        []string // injected as --env pairs (grant env + proxy env + GW ip)
	StageDir   string
	PromptFile string
	MemoryGB   int
	CPUs       int
}

// BuildRunArgs returns the argv that follows the `container` binary name.
func BuildRunArgs(s Spec) []string {
	args := []string{
		"run", "--rm",
		"--name", "task-" + s.TaskID,
		// entrypoint.sh starts as root to install the nft pin, then drops to
		// the agent user via setpriv (no-new-privs + empty cap bounding set;
		// see image/drop-agent.sh). Don't pass --user here, or nft can't flush.
		"--cap-add", "CAP_NET_ADMIN",
		"--memory", fmt.Sprintf("%dG", s.MemoryGB),
		"--cpus", fmt.Sprintf("%d", s.CPUs),
		"--network", s.Network,
	}
	for _, e := range s.Env {
		args = append(args, "--env", e)
	}
	args = append(args,
		"--env", "TASK_PROMPT_FILE="+s.PromptFile,
		// Apple container treats "readonly" as a presence flag; setting
		// readonly=false still mounts read-only. Omit it entirely for rw.
		"--mount", fmt.Sprintf("type=bind,source=%s,target=/work", s.StageDir),
		s.ImageRef,
		"/usr/local/bin/entrypoint.sh",
	)
	return args
}

// verifyScript is the in-VM bootstrap for a verification command. Root
// installs a DENY-ALL nft pin (loopback only — no gateway, no squid: the
// verifier's claim is "no network", strictly tighter than the agent's
// allowlist), then execs the command through drop-agent.sh so repo code
// runs unprivileged and cannot flush the pin (same A2 mechanism the agent
// VM relies on). HOME must be the agent user's writable home (v0.6.6 #198)
// or toolchains that write under $HOME fail spuriously.
const verifyScript = `set -e
nft -f - <<'EOF'
table inet verify_pin {
  chain input   { type filter hook input   priority 0; policy drop; iifname "lo" accept; }
  chain forward { type filter hook forward priority 0; policy drop; }
  chain output  { type filter hook output  priority 0; policy drop; oifname "lo" accept; }
}
EOF
export HOME=/home/agent
cd /work
exec /usr/local/bin/drop-agent "$@"
`

// VerifySpec describes one verification command's VM run.
type VerifySpec struct {
	TaskID    string
	Network   string
	ImageRef  string
	VerifyDir string   // sealed staged-tree export, mounted rw at /work
	Argv      []string // the verification command, passed as positionals
	MemoryGB  int
	CPUs      int
}

// VerifyContainerName is the container name for a task's verifier VMs.
// Distinct from "task-<id>" so kill/reap paths for the two never collide.
func VerifyContainerName(taskID string) string { return "verify-" + taskID }

// BuildVerifyArgs returns the argv (after the `container` binary) for one
// verification command. No credential or proxy env is ever injected here —
// the verifier's evidence value depends on it having nothing to leak.
func BuildVerifyArgs(s VerifySpec) []string {
	args := []string{
		"run", "--rm",
		"--name", VerifyContainerName(s.TaskID),
		"--cap-add", "CAP_NET_ADMIN",
		"--memory", fmt.Sprintf("%dG", s.MemoryGB),
		"--cpus", fmt.Sprintf("%d", s.CPUs),
		"--network", s.Network,
		"--env", "HOME=/home/agent",
		"--mount", fmt.Sprintf("type=bind,source=%s,target=/work", s.VerifyDir),
		"--entrypoint", "/bin/sh",
		s.ImageRef,
		"-c", verifyScript, "sh",
	}
	return append(args, s.Argv...)
}
