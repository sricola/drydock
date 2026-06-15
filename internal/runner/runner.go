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
		// entrypoint.sh starts as root to install the nft pin, then drops to
		// the agent user via gosu. Don't pass --user here, or nft can't flush.
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
