package runner

import (
	"slices"
	"testing"
)

func TestBuildRunArgs_Contains(t *testing.T) {
	args := BuildRunArgs(Spec{
		TaskID:     "abc123",
		Network:    "drydock-egress",
		ImageRef:   "drydock-sandbox:latest",
		Env:        []string{"ANTHROPIC_BASE_URL=http://gw:8088", "DRYDOCK_GW_IP=192.168.64.1"},
		StageDir:   "/tmp/broker/stage/abc123",
		PromptFile: "/work/.task/prompt.txt",
		MemoryGB:   4,
		CPUs:       4,
	})

	for _, want := range [][]string{
		{"run", "--rm"},
		{"--name", "task-abc123"},
		{"--cap-add", "CAP_NET_ADMIN"},
		{"--memory", "4G"},
		{"--cpus", "4"},
		{"--network", "drydock-egress"},
		{"--env", "ANTHROPIC_BASE_URL=http://gw:8088"},
		{"--env", "DRYDOCK_GW_IP=192.168.64.1"},
		{"--env", "TASK_PROMPT_FILE=/work/.task/prompt.txt"},
		{"--mount", "type=bind,source=/tmp/broker/stage/abc123,target=/work"},
	} {
		if !containsPair(args, want[0], want[1]) {
			t.Errorf("args missing %q %q\n got: %v", want[0], want[1], args)
		}
	}
	if args[len(args)-1] != "/usr/local/bin/entrypoint.sh" {
		t.Errorf("last arg = %q, want entrypoint.sh", args[len(args)-1])
	}
	if !slices.Contains(args, "drydock-sandbox:latest") {
		t.Errorf("args missing image ref")
	}
}

func containsPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}
