// Package runner builds the `container` CLI argv for a sandbox task.
package runner

import "fmt"

type Spec struct {
	TaskID     string
	Network    string
	ImageRef   string
	APIKey     string
	StageDir   string
	PromptFile string
	MemoryGB   int
	CPUs       int
}

// BuildRunArgs returns the argv that follows the `container` binary name.
func BuildRunArgs(s Spec) []string {
	return []string{
		"run", "--rm",
		"--name", "task-" + s.TaskID,
		"--user", "agent",
		// The entrypoint installs an nft egress firewall as root before dropping
		// to the agent; nft requires CAP_NET_ADMIN. Without it the firewall fails
		// to install and (under entrypoint's set -e) the task aborts fail-closed.
		"--cap-add", "CAP_NET_ADMIN",
		"--memory", fmt.Sprintf("%dG", s.MemoryGB),
		"--cpus", fmt.Sprintf("%d", s.CPUs),
		"--network", s.Network,
		"--env", "ANTHROPIC_API_KEY=" + s.APIKey,
		"--env", "TASK_PROMPT_FILE=" + s.PromptFile,
		"--mount", fmt.Sprintf("type=bind,source=%s,target=/work,readonly=false", s.StageDir),
		s.ImageRef,
		"/usr/local/bin/entrypoint.sh",
	}
}
