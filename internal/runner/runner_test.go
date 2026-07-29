package runner

import (
	"slices"
	"strings"
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

func TestBuildVerifyArgs_ShapeAndContainment(t *testing.T) {
	args := BuildVerifyArgs(VerifySpec{
		TaskID: "0123456789abcdef0123456789abcdef", Network: "drydock-egress",
		ImageRef: "drydock-sandbox:latest", VerifyDir: "/stage/verify",
		Argv: []string{"go", "test", "./..."}, MemoryGB: 4, CPUs: 4,
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--name verify-0123456789abcdef0123456789abcdef",
		"--cap-add CAP_NET_ADMIN", // root installs the deny-all pin, then drops
		"--mount type=bind,source=/stage/verify,target=/work",
		"policy drop",                  // the inline nft pin
		"/usr/local/bin/drop-agent.sh", // privilege drop before repo code
		"HOME=/home/agent",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("verify argv missing %q:\n%s", want, joined)
		}
	}
	// The command argv must arrive as positional args after the sh -c script
	// (never interpolated into the script string — shell-injection surface).
	// Exact tail shape: ..., "-c", <script>, "sh", "go", "test", "./..."
	n := len(args)
	if n < 4 || args[n-4] != "sh" || args[n-3] != "go" || args[n-2] != "test" || args[n-1] != "./..." {
		t.Errorf("argv tail = %v, want [... sh go test ./...]", args[max(0, n-4):])
	}
	for _, a := range args {
		if strings.Contains(a, "tok_") || strings.Contains(a, "PROXY") ||
			strings.Contains(a, "AUTH_TOKEN") || strings.Contains(a, "API_KEY") {
			t.Errorf("verify argv leaks credential/proxy material: %q", a)
		}
	}
}
