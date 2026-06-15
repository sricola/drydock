// Package runner builds the `container` CLI argv for a sandbox task.
package runner

import "fmt"

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
		"--user", "agent",
		// nft egress firewall installs as root in the entrypoint; needs CAP_NET_ADMIN.
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
		"--mount", fmt.Sprintf("type=bind,source=%s,target=/work,readonly=false", s.StageDir),
		s.ImageRef,
		"/usr/local/bin/entrypoint.sh",
	)
	return args
}
