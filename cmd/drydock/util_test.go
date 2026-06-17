package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestAuditDir_DefaultAndOverride(t *testing.T) {
	t.Setenv("AUDIT_ROOT", "")
	// The resolver prefers ~/.drydock/audit and falls back to
	// /tmp/broker/audit only when the new dir is empty AND the legacy
	// path has files. Don't hard-code either path here (CI runner's home
	// is /home/runner, workstations vary); just assert one of the two
	// shapes. The new-default contract is unit-tested in internal/config
	// (TestDefaults_StateDirsUnderHomeNotTmp).
	got := auditDir()
	if !strings.HasSuffix(got, "/.drydock/audit") && got != "/tmp/broker/audit" {
		t.Errorf("default auditDir = %q; expected …/.drydock/audit (or legacy /tmp/broker/audit)", got)
	}
	t.Setenv("AUDIT_ROOT", "/custom/dir")
	if got := auditDir(); got != "/custom/dir" {
		t.Errorf("override auditDir = %q", got)
	}
}

func TestPathBuilders(t *testing.T) {
	t.Setenv("AUDIT_ROOT", "/audit")
	if got := diffPath("ab12"); got != "/audit/ab12.diff" {
		t.Errorf("diffPath = %q", got)
	}
	if got := auditPath("ab12"); got != "/audit/ab12.jsonl" {
		t.Errorf("auditPath = %q", got)
	}
}

func TestRelAge(t *testing.T) {
	cases := []struct {
		ago    time.Duration
		expect string
	}{
		{2 * time.Second, "2s"},
		{45 * time.Second, "45s"},
		{2 * time.Minute, "2m"},
		{90 * time.Minute, "1h"},
		{36 * time.Hour, "1d"},
		{72 * time.Hour, "3d"},
	}
	for _, tc := range cases {
		got := relAge(time.Now().Add(-tc.ago))
		if got != tc.expect {
			t.Errorf("relAge(now - %v) = %q, want %q", tc.ago, got, tc.expect)
		}
	}
}

func TestShortDur(t *testing.T) {
	cases := []struct {
		ms     int64
		expect string
	}{
		{42, "42ms"},
		{900, "900ms"},
		{2300, "2.3s"},
		{37000, "37s"},
		{125000, "2m05s"},
	}
	for _, tc := range cases {
		if got := shortDur(tc.ms); got != tc.expect {
			t.Errorf("shortDur(%d) = %q, want %q", tc.ms, got, tc.expect)
		}
	}
}

func TestShorten_KeepsTail(t *testing.T) {
	// `git@github.com:owner/repo` → keep "owner/repo".
	if got := shorten("git@github.com:owner/repo", 40); got != "owner/repo" {
		t.Errorf("shorten ssh form = %q", got)
	}
	// Long URL → truncated with ellipsis.
	long := "https://very.long.gitlab.example.com/group/sub/project"
	if got := shorten(long, 20); !strings.HasSuffix(got, "…") {
		t.Errorf("shorten long should end with ellipsis: %q", got)
	}
}

func TestSingleLine(t *testing.T) {
	if got := singleLine("one line"); got != "one line" {
		t.Errorf("singleLine no-newline = %q", got)
	}
	if got := singleLine("first\nsecond\nthird"); got != "first …" {
		t.Errorf("singleLine multi = %q", got)
	}
}

func TestFormatExtras(t *testing.T) {
	if got := formatExtras(nil); got != "(no hosts?)" {
		t.Errorf("empty = %q", got)
	}
	got := formatExtras([]domain{
		{Host: "a.example.com", Ports: []int{443}},
		{Host: "b.example.com", Ports: []int{80, 8080}},
	})
	want := "a.example.com:443 b.example.com:80,8080"
	if got != want {
		t.Errorf("formatExtras = %q, want %q", got, want)
	}
}

func TestSiblingOf(t *testing.T) {
	if got := siblingOf("/usr/local/bin/drydock", "brokerd"); got != "/usr/local/bin/brokerd" {
		t.Errorf("siblingOf path = %q", got)
	}
	// No directory separator: fall back to bare name.
	if got := siblingOf("drydock", "brokerd"); got != "brokerd" {
		t.Errorf("siblingOf bare = %q", got)
	}
}

func TestIsNoSuchContainer(t *testing.T) {
	for _, in := range []string{
		"Error: container task-abc not found",
		"no such container",
		"task does not exist",
	} {
		if !isNoSuchContainer(in) {
			t.Errorf("want match: %q", in)
		}
	}
	if isNoSuchContainer("Error: rate limited") {
		t.Errorf("unrelated error should not match")
	}
	if isNoSuchContainer("") {
		t.Errorf("empty should not match")
	}
}

// indexOf / contains are tiny helpers — verify they don't fence-post on the
// edges (empty needle, needle == haystack, etc.).
func TestContains_Edges(t *testing.T) {
	if !contains("abc", "abc") {
		t.Error("equal must match")
	}
	if contains("ab", "abc") {
		t.Error("shorter haystack must not match")
	}
	if !contains("xyzfoo", "foo") {
		t.Error("suffix must match")
	}
}

func TestEnv_Smoke(t *testing.T) {
	// Used by client/status code paths — ensure helpers don't panic on
	// pathological input.
	t.Setenv("AUDIT_ROOT", "")
	_ = auditDir()
	// silence unused-import lint
	_ = os.PathSeparator
}
