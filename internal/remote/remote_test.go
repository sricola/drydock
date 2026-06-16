package remote

import "testing"

func TestAdapterFor(t *testing.T) {
	cases := []struct {
		repoRef  string
		platform string
		want     string
	}{
		// Explicit platform wins over URL hostname.
		{"git@gitlab.mycorp.com:o/r", "gitlab", "gitlab"},
		{"git@github.com:o/r", "gitlab", "gitlab"},
		{"https://github.com/o/r", "none", "push-only"},
		{"https://github.com/o/r", "push-only", "push-only"},
		{"", "github", "github"},

		// Autodetect by hostname when platform is empty.
		{"https://github.com/sricola/drydock", "", "github"},
		{"git@github.com:sricola/drydock.git", "", "github"},
		{"ssh://git@github.com/sricola/drydock", "", "github"},
		{"https://gitlab.com/group/project", "", "gitlab"},
		{"git@gitlab.com:group/project.git", "", "gitlab"},

		// Self-hosted, unknown vendor → push-only when no platform.
		// (Self-hosted GitLab callers must set platform="gitlab".)
		{"git@gitlab.mycorp.com:group/project", "", "push-only"},
		{"https://git.kernel.org/torvalds/linux", "", "push-only"},
		{"git@bitbucket.org:o/r", "", "push-only"},

		// Unknown platform string falls through to autodetect.
		{"https://github.com/o/r", "huggingface", "github"},
	}
	for _, tc := range cases {
		got := AdapterFor(tc.repoRef, tc.platform).Name()
		if got != tc.want {
			t.Errorf("AdapterFor(%q, %q) = %q, want %q", tc.repoRef, tc.platform, got, tc.want)
		}
	}
}

func TestPushOnly_NeverErrors(t *testing.T) {
	if err := (PushOnlyAdapter{}).OpenRequest("/tmp", "feature/x", nil); err != nil {
		t.Errorf("PushOnly must be a no-op success: %v", err)
	}
}
