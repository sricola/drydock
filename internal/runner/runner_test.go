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

func TestBuildSetupArgs_ShapeAndContainment(t *testing.T) {
	args := BuildSetupArgs(SetupSpec{
		TaskID: "0123456789abcdef0123456789abcdef", Network: "drydock-egress",
		ImageRef: "drydock-sandbox:latest", StageDir: "/stage/abc123",
		Env: []string{
			"HTTPS_PROXY=http://192.168.64.1:3128",
			"HTTP_PROXY=http://192.168.64.1:3128",
			"DRYDOCK_GW_IP=192.168.64.1",
		},
		Argv: []string{"npm", "ci"}, MemoryGB: 4, CPUs: 4,
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--name setup-0123456789abcdef0123456789abcdef",
		"--cap-add CAP_NET_ADMIN",                             // root installs the egress pin, then drops
		"--mount type=bind,source=/stage/abc123,target=/work", // live stage, rw
		`init-firewall.sh "$DRYDOCK_GW_IP" 3128`,              // squid-only egress pin
		"/usr/local/bin/drop-agent",                           // privilege drop before repo code
		"HOME=/home/agent",
		"--env HTTPS_PROXY=http://192.168.64.1:3128", // egress must work
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("setup argv missing %q:\n%s", want, joined)
		}
	}
	// Setup gets squid-only egress, NOT the verifier's deny-all pin.
	if strings.Contains(joined, "policy drop") {
		t.Errorf("setup argv contains the verifier's deny-all pin:\n%s", joined)
	}
	// Setup must NOT be able to reach the model credential gateway (:8088):
	// it has no bearer and no LLM business; :8088 is dropped as a backstop.
	if strings.Contains(joined, "8088") {
		t.Errorf("setup argv allows the model gateway port 8088; want squid-only:\n%s", joined)
	}
	// The command argv must arrive as positional args after the sh -c script
	// (never interpolated into the script string — shell-injection surface).
	// Exact tail shape: ..., "-c", <script>, "sh", "npm", "ci"
	n := len(args)
	if n < 3 || args[n-3] != "sh" || args[n-2] != "npm" || args[n-1] != "ci" {
		t.Errorf("argv tail = %v, want [... sh npm ci]", args[max(0, n-3):])
	}
	// Setup can never spend: given a proxy-only Env, no arg may carry a grant
	// bearer or credential-shaped material.
	for _, a := range args {
		for _, leak := range []string{"tok_", "AUTH_TOKEN", "API_KEY", "Bearer"} {
			if strings.Contains(a, leak) {
				t.Errorf("setup argv leaks credential material %q: %q", leak, a)
			}
		}
	}
}

func TestBuildSetupArgs_CacheMountRW(t *testing.T) {
	spec := SetupSpec{
		TaskID: "abc123", Network: "drydock-egress",
		ImageRef: "drydock-sandbox:latest", StageDir: "/stage/abc123",
		Argv: []string{"npm", "ci"}, MemoryGB: 4, CPUs: 4,
	}

	// No CacheDir → no cache mount at all (backward compat).
	joined := strings.Join(BuildSetupArgs(spec), " ")
	if strings.Contains(joined, "target=/deps") {
		t.Errorf("setup argv has a cache mount with empty CacheDir:\n%s", joined)
	}

	spec.CacheDir = "/var/cache/drydock/deps"
	joined = strings.Join(BuildSetupArgs(spec), " ")
	// Setup writes the cache: the mount must be RW. Apple container treats
	// "readonly" as a presence flag, so RW means the flag is absent entirely.
	if !strings.Contains(joined, "--mount type=bind,source=/var/cache/drydock/deps,target=/deps") {
		t.Errorf("setup argv missing rw cache mount:\n%s", joined)
	}
	if strings.Contains(joined, "target=/deps,readonly") {
		t.Errorf("setup cache mount is readonly; setup must be able to write the cache:\n%s", joined)
	}
}

func TestBuildRunArgs_CacheMountRO(t *testing.T) {
	spec := Spec{
		TaskID: "abc123", Network: "drydock-egress",
		ImageRef: "drydock-sandbox:latest", StageDir: "/stage/abc123",
		PromptFile: "/work/.task/prompt.txt", MemoryGB: 4, CPUs: 4,
	}

	// No CacheDir → no cache mount at all (backward compat).
	joined := strings.Join(BuildRunArgs(spec), " ")
	if strings.Contains(joined, "target=/deps") {
		t.Errorf("agent argv has a cache mount with empty CacheDir:\n%s", joined)
	}

	spec.CacheDir = "/var/cache/drydock/deps"
	joined = strings.Join(BuildRunArgs(spec), " ")
	// SECURITY: the agent's view of the shared cache MUST be read-only.
	// Removing ",readonly" would let agent-run code write the cache other
	// tasks consume — cache poisoning. This assertion is the regression gate.
	if !strings.Contains(joined, "--mount type=bind,source=/var/cache/drydock/deps,target=/deps,readonly") {
		t.Errorf("agent argv missing READONLY cache mount (agent must not write the shared cache):\n%s", joined)
	}
}

func TestCacheEnv_PathsMatchMountTarget(t *testing.T) {
	env := CacheEnv()
	want := map[string]bool{
		"npm_config_cache=/deps/npm": false,
		"GOMODCACHE=/deps/go/mod":    false,
		"PIP_CACHE_DIR=/deps/pip":    false,
		"CARGO_HOME=/deps/cargo":     false,
	}
	for _, e := range env {
		if _, ok := want[e]; ok {
			want[e] = true
		}
		// Every cache path must live under the mount target, or the tools
		// would write somewhere the cache mount doesn't cover.
		_, val, ok := strings.Cut(e, "=")
		if !ok || !strings.HasPrefix(val, cacheMountTarget+"/") {
			t.Errorf("CacheEnv entry %q not under mount target %q", e, cacheMountTarget)
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("CacheEnv missing %q; got %v", k, env)
		}
	}
	if cacheMountTarget != "/deps" {
		t.Errorf("cacheMountTarget = %q, want /deps", cacheMountTarget)
	}
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
		"policy drop",               // the inline nft pin
		"/usr/local/bin/drop-agent", // privilege drop before repo code
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
