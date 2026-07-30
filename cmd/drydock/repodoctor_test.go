package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"drydock/internal/broker"
	"drydock/internal/config"
	"drydock/internal/egress"
)

// testLimits are the production stage bounds; individual tests shrink them to
// exercise the over-cap paths without creating gigabytes or 200k files.
func testLimits() stageLimits {
	return stageLimits{MaxBytes: broker.DefaultMaxStageBytes, MaxFiles: broker.DefaultMaxStageFiles}
}

// testEgress mirrors internal/defaults/egress.yaml: the npm/pypi/go registry
// hosts are allowlisted out of the box; rust/ruby are not.
func testEgress() egress.Config {
	var eg egress.Config
	for _, h := range []string{
		"api.anthropic.com", "registry.npmjs.org",
		"pypi.org", "files.pythonhosted.org",
		"proxy.golang.org", "sum.golang.org",
	} {
		eg.Default.Domains = append(eg.Default.Domains, egress.Domain{Host: h, Ports: []int{443}})
	}
	return eg
}

func writeRepoFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findCheck(t *testing.T, checks []repoCheck, label string) repoCheck {
	t.Helper()
	for _, c := range checks {
		if c.Label == label {
			return c
		}
	}
	t.Fatalf("no %q check in %+v", label, checks)
	return repoCheck{}
}

func hasCheck(checks []repoCheck, label string) bool {
	for _, c := range checks {
		if c.Label == label {
			return true
		}
	}
	return false
}

func TestDiagnoseRepo_SizeUnderCapPasses(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, "main.go", "package main\n")
	writeRepoFile(t, dir, "README.md", "hi\n")

	checks := diagnoseRepo(dir, config.Defaults(), testEgress(), testLimits())
	c := findCheck(t, checks, "size")
	if !c.OK || c.Warn {
		t.Fatalf("size check = %+v, want pass", c)
	}
	if !strings.Contains(c.Detail, "2 files") {
		t.Errorf("size Detail %q should report the file count", c.Detail)
	}
}

func TestDiagnoseRepo_SizeOverFileCapBlocks(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		writeRepoFile(t, dir, n, "x")
	}
	lim := testLimits()
	lim.MaxFiles = 2

	c := findCheck(t, diagnoseRepo(dir, config.Defaults(), testEgress(), lim), "size")
	if c.OK {
		t.Fatalf("size check = %+v, want blocker (OK=false)", c)
	}
	if !strings.Contains(c.Detail, "exceeds") {
		t.Errorf("size Detail %q should say the cap is exceeded", c.Detail)
	}
}

func TestDiagnoseRepo_SizeOverByteCapBlocks(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, "big.bin", strings.Repeat("x", 100))
	lim := testLimits()
	lim.MaxBytes = 10

	c := findCheck(t, diagnoseRepo(dir, config.Defaults(), testEgress(), lim), "size")
	if c.OK {
		t.Fatalf("size check = %+v, want blocker (OK=false)", c)
	}
}

func TestDiagnoseRepo_RustToolchainWarns(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, "Cargo.toml", "[package]\nname = \"x\"\n")

	checks := diagnoseRepo(dir, config.Defaults(), testEgress(), testLimits())
	tc := findCheck(t, checks, "toolchain")
	if !tc.OK || !tc.Warn {
		t.Fatalf("toolchain check = %+v, want warn", tc)
	}
	if !strings.Contains(tc.Detail, "rust") {
		t.Errorf("toolchain Detail %q should name rust", tc.Detail)
	}
	ec := findCheck(t, checks, "egress")
	if !ec.OK || !ec.Warn {
		t.Fatalf("egress check = %+v, want warn", ec)
	}
	if !strings.Contains(ec.Detail, "crates.io") {
		t.Errorf("egress Detail %q should name crates.io", ec.Detail)
	}
}

func TestDiagnoseRepo_NodeRepoUnderDefaultsPasses(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, "package.json", "{}\n")

	checks := diagnoseRepo(dir, config.Defaults(), testEgress(), testLimits())
	tc := findCheck(t, checks, "toolchain")
	if !tc.OK || tc.Warn {
		t.Fatalf("toolchain check = %+v, want pass", tc)
	}
	if !strings.Contains(tc.Detail, "node") {
		t.Errorf("toolchain Detail %q should name node", tc.Detail)
	}
	ec := findCheck(t, checks, "egress")
	if !ec.OK || ec.Warn {
		t.Fatalf("egress check = %+v, want pass", ec)
	}
}

func TestDiagnoseRepo_CustomNpmRegistryWarns(t *testing.T) {
	const token = "npm_SECRETTOKEN123abc"
	dir := t.TempDir()
	writeRepoFile(t, dir, "package.json", "{}\n")
	writeRepoFile(t, dir, ".npmrc",
		"registry=https://nexus.corp/npm/\n"+
			"//nexus.corp/npm/:_authToken="+token+"\n")

	checks := diagnoseRepo(dir, config.Defaults(), testEgress(), testLimits())
	ec := findCheck(t, checks, "egress")
	if !ec.OK || !ec.Warn {
		t.Fatalf("egress check = %+v, want warn", ec)
	}
	if !strings.Contains(ec.Detail, "nexus.corp") {
		t.Errorf("egress Detail %q should name nexus.corp", ec.Detail)
	}
	for _, c := range checks {
		if strings.Contains(c.Detail, token) {
			t.Fatalf("check %q Detail echoes the .npmrc auth token: %q", c.Label, c.Detail)
		}
	}
}

func TestDiagnoseRepo_YarnBerryRegistryWarns(t *testing.T) {
	const token = "yarn_SECRETTOKEN456def"
	dir := t.TempDir()
	writeRepoFile(t, dir, "package.json", "{}\n")
	writeRepoFile(t, dir, ".yarnrc.yml",
		"npmRegistryServer: \"https://nexus.corp/npm/\"\n"+
			"npmAuthToken: \""+token+"\"\n")

	checks := diagnoseRepo(dir, config.Defaults(), testEgress(), testLimits())
	ec := findCheck(t, checks, "egress")
	if !ec.OK || !ec.Warn {
		t.Fatalf("egress check = %+v, want warn (never a blocker)", ec)
	}
	if !strings.Contains(ec.Detail, "nexus.corp") {
		t.Errorf("egress Detail %q should name nexus.corp", ec.Detail)
	}
	if !strings.Contains(ec.Detail, ".yarnrc.yml") {
		t.Errorf("egress Detail %q should name the .yarnrc.yml source file", ec.Detail)
	}
	for _, c := range checks {
		if strings.Contains(c.Detail, token) {
			t.Fatalf("check %q Detail echoes the .yarnrc.yml auth token: %q", c.Label, c.Detail)
		}
	}
}

func TestDiagnoseRepo_GradleKtsToolchainWarns(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, "build.gradle.kts", "plugins { java }\n")

	checks := diagnoseRepo(dir, config.Defaults(), testEgress(), testLimits())
	tc := findCheck(t, checks, "toolchain")
	if !tc.OK || !tc.Warn {
		t.Fatalf("toolchain check = %+v, want warn (never a blocker)", tc)
	}
	if !strings.Contains(tc.Detail, "java") {
		t.Errorf("toolchain Detail %q should name java", tc.Detail)
	}
	if !strings.Contains(tc.Detail, "build.gradle.kts") {
		t.Errorf("toolchain Detail %q should name the build.gradle.kts manifest", tc.Detail)
	}
}

func TestCustomRegistryHosts_PipConfSubdir(t *testing.T) {
	repo := t.TempDir()
	writeRepoFile(t, repo, ".pip/pip.conf",
		"[global]\nindex-url = https://pypi.corp/simple\n")

	got := customRegistryHosts(repo)
	if len(got) != 1 || got[0].host != "pypi.corp" || got[0].file != ".pip/pip.conf" {
		t.Fatalf("customRegistryHosts = %+v, want pypi.corp from .pip/pip.conf", got)
	}
}

// TestCustomRegistryHosts_SkipsSymlinkedPipDir extends the never-follow-symlink
// invariant to the nested .pip/pip.conf path: O_NOFOLLOW only guards the final
// component, so a symlinked .pip directory pointing outside the repo must be
// refused before the open.
func TestCustomRegistryHosts_SkipsSymlinkedPipDir(t *testing.T) {
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "d", "pip.conf"),
		[]byte("[global]\nindex-url = https://evil.example/simple\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "d"), filepath.Join(repo, ".pip")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got := customRegistryHosts(repo); len(got) != 0 {
		t.Fatalf("pip.conf under a symlinked .pip dir was read: %+v", got)
	}
}

func TestDiagnoseRepo_BlockedPathPreviewBlocks(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, ".github/workflows/ci.yml", "on: push\n")
	writeRepoFile(t, dir, "main.go", "package main\n")
	cfg := config.Defaults()
	cfg.DiffPolicy.BlockedPaths = []string{".github/workflows/**"}

	c := findCheck(t, diagnoseRepo(dir, cfg, testEgress(), testLimits()), "diff_policy")
	if c.OK {
		t.Fatalf("diff_policy check = %+v, want blocker (OK=false)", c)
	}
	if !strings.Contains(c.Detail, "1 file") {
		t.Errorf("diff_policy Detail %q should count 1 blocked file", c.Detail)
	}
}

func TestDiagnoseRepo_SecondLookPreviewWarns(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, "app/Dockerfile", "FROM scratch\n")
	cfg := config.Defaults()
	cfg.DiffPolicy.SecondLookPaths = []string{"**/Dockerfile"}

	c := findCheck(t, diagnoseRepo(dir, cfg, testEgress(), testLimits()), "diff_policy")
	if !c.OK || !c.Warn {
		t.Fatalf("diff_policy check = %+v, want warn", c)
	}
}

func TestDiagnoseRepo_NoDiffPolicyNoCheck(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, "main.go", "package main\n")

	checks := diagnoseRepo(dir, config.Defaults(), testEgress(), testLimits())
	if hasCheck(checks, "diff_policy") {
		t.Errorf("diff_policy check emitted with an empty DiffPolicy: %+v", checks)
	}
}

func TestDiagnoseRepo_SkipsGitDir(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, "main.go", "package main\n")
	writeRepoFile(t, dir, ".git/objects/pack/big.pack", strings.Repeat("x", 1<<20))
	lim := testLimits()
	lim.MaxBytes = 1000 // far below the .git blob; only main.go should count

	c := findCheck(t, diagnoseRepo(dir, config.Defaults(), testEgress(), lim), "size")
	if !c.OK {
		t.Fatalf("size check = %+v, want pass (.git must be skipped)", c)
	}
	if !strings.Contains(c.Detail, "1 file") {
		t.Errorf("size Detail %q should count only the 1 non-.git file", c.Detail)
	}
}

// TestCustomRegistryHosts_SkipsSymlinkedNpmrc pins the walk's never-follow-
// symlink invariant onto the registry-config reader too: a repo whose .npmrc
// is a symlink pointing outside the repo must be skipped, not read — otherwise
// a hostile repo could make doctor read (and partially reveal hosts from) an
// arbitrary out-of-repo file.
func TestCustomRegistryHosts_SkipsSymlinkedNpmrc(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "out-of-repo-npmrc")
	if err := os.WriteFile(target, []byte("registry=https://evil.example/npm/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.Symlink(target, filepath.Join(repo, ".npmrc")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got := customRegistryHosts(repo); len(got) != 0 {
		t.Fatalf("symlinked .npmrc was read: %+v", got)
	}
}

// TestRegistryHostFromLine pins the assignment-only grammar: real registry
// assignments yield their hostname; comments, auth-token lines, and prose
// that merely contains the word "registry" next to a URL yield "".
func TestRegistryHostFromLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"npmrc assignment", "registry=https://nexus.corp/npm/", "nexus.corp"},
		{"npmrc scoped assignment", "@myscope:registry=https://scoped.corp/npm/", "scoped.corp"},
		{"pip index-url spaced", "index-url = https://pypi.corp/simple", "pypi.corp"},
		{"cargo source table entry", `registry = "https://my-registry.example/index"`, "my-registry.example"},
		{"cargo registries index entry", `index = "https://crates.corp/git/index"`, "crates.corp"},
		{"yarn classic space form", `registry "https://yarn.corp/"`, "yarn.corp"},
		{"yarn berry npmRegistryServer", `npmRegistryServer: "https://nexus.corp/npm/"`, "nexus.corp"},
		{"yarn berry npmRegistryServer indented scope", `  npmRegistryServer: "https://scoped.corp/npm/"`, "scoped.corp"},
		{"yarn berry auth token line", `npmAuthToken: "yarn_SECRET"`, ""},
		{"yaml key that is not npmRegistryServer", `homepage: "https://x.example/"`, ""},
		{"comment mentioning a registry url", "# see https://x.example/registry-help", ""},
		{"semicolon comment", "; registry=https://x.example/", ""},
		{"prose containing registry and a url", "the registry lives at https://x.example/path", ""},
		{"auth token line", "//nexus.corp/npm/:_authToken=npm_SECRET", ""},
		{"unrelated assignment", "always-auth=true", ""},
		{"registry assignment without url", "registry=local", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := registryHostFromLine(c.line); got != c.want {
				t.Errorf("registryHostFromLine(%q) = %q, want %q", c.line, got, c.want)
			}
		})
	}
}

func TestProductionStageLimits(t *testing.T) {
	cfg := config.Defaults()

	cfg.StageQuotaGB = 2 // below the 4 GiB polling default -> quota wins
	if got := productionStageLimits(cfg); got.MaxBytes != 2<<30 || got.MaxFiles != broker.DefaultMaxStageFiles {
		t.Errorf("limits(quota=2) = %+v", got)
	}
	cfg.StageQuotaGB = 100 // above the polling default -> default wins
	if got := productionStageLimits(cfg); got.MaxBytes != broker.DefaultMaxStageBytes {
		t.Errorf("limits(quota=100) = %+v", got)
	}
	cfg.StageQuotaGB = 0 // quota image disabled -> polling default only
	if got := productionStageLimits(cfg); got.MaxBytes != broker.DefaultMaxStageBytes {
		t.Errorf("limits(quota=0) = %+v", got)
	}
}
