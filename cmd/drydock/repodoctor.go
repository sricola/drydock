package main

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"drydock/internal/broker"
	"drydock/internal/config"
	"drydock/internal/egress"
	"drydock/internal/globmatch"
)

// repoCheck is one preflight finding about a repo. OK=true && Warn=false is a
// pass; Warn=true is a warning (task can run but will likely hit friction);
// OK=false is a hard blocker (the task is guaranteed to fail — exit non-zero).
type repoCheck struct {
	Label  string
	OK     bool
	Warn   bool
	Detail string
}

// stageLimits are the stage-size bounds diagnoseRepo compares the repo
// against. A parameter (rather than consts) so tests can exercise the
// over-cap paths with tiny caps; production uses productionStageLimits.
type stageLimits struct {
	MaxBytes int64
	MaxFiles int
}

// productionStageLimits derives the effective per-task stage bounds: the
// tighter of the macOS quota image (stage_quota_gb, when set) and the polling
// stage guard's defaults. Whichever is smaller is the wall the task hits first.
func productionStageLimits(cfg *config.Config) stageLimits {
	maxBytes := broker.DefaultMaxStageBytes
	if cfg.StageQuotaGB > 0 {
		if q := int64(cfg.StageQuotaGB) << 30; q < maxBytes {
			maxBytes = q
		}
	}
	return stageLimits{MaxBytes: maxBytes, MaxFiles: broker.DefaultMaxStageFiles}
}

// manifestLangs maps dependency-manifest filenames to the language toolchain
// and package ecosystem they imply. Filenames mirror the dependency-manifest
// list in trustbrief.classifyPath.
var manifestLangs = map[string]struct{ lang, eco string }{
	"package.json":     {"node", "npm"},
	"go.mod":           {"go", "go"},
	"requirements.txt": {"python", "pip"},
	"pyproject.toml":   {"python", "pip"},
	"Cargo.toml":       {"rust", "crates"},
	"Gemfile":          {"ruby", "rubygems"},
	"pom.xml":          {"java", "maven"},
	"build.gradle":     {"java", "maven"},
}

// ecoRegistryHosts is the default registry host set each package ecosystem
// needs egress to. Ecosystems absent here (maven) have no entry in the
// default allowlist either; their toolchain warning already covers them.
var ecoRegistryHosts = map[string][]string{
	"npm":      {"registry.npmjs.org"},
	"pip":      {"pypi.org", "files.pythonhosted.org"},
	"go":       {"proxy.golang.org", "sum.golang.org"},
	"crates":   {"crates.io", "static.crates.io"},
	"rubygems": {"rubygems.org"},
}

// diagnoseRepo is the pure, container-free repo preflight: one bounded walk
// of dir plus a few byte-capped config-file reads, producing checks a
// submitter can act on before spending a container boot or API budget.
// It never echoes repo file contents into a Detail — registry config files
// can hold auth tokens — only hostnames extracted via url.Parse.
func diagnoseRepo(dir string, cfg *config.Config, eg egress.Config, limits stageLimits) []repoCheck {
	var (
		totalBytes int64
		fileCount  int
		over       bool
	)
	langManifest := map[string]string{} // lang -> first manifest rel path seen
	ecos := map[string]bool{}
	blocked, second := 0, 0
	pol := cfg.DiffPolicy
	policyActive := len(pol.BlockedPaths) > 0 || len(pol.SecondLookPaths) > 0

	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best effort; ignore transient read races (per stageOverLimit)
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil // never follow symlinks; count regular files only
		}
		fileCount++
		if info, e := d.Info(); e == nil {
			totalBytes += info.Size()
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			rel = d.Name()
		}
		rel = filepath.ToSlash(rel)
		if m, ok := manifestLangs[d.Name()]; ok {
			if _, seen := langManifest[m.lang]; !seen {
				langManifest[m.lang] = rel
			}
			ecos[m.eco] = true
		}
		if policyActive {
			for _, pat := range pol.BlockedPaths {
				if globmatch.Match(pat, rel) {
					blocked++
					break
				}
			}
			for _, pat := range pol.SecondLookPaths {
				if globmatch.Match(pat, rel) {
					second++
					break
				}
			}
		}
		if totalBytes > limits.MaxBytes || fileCount > limits.MaxFiles {
			over = true
			return filepath.SkipAll // ceiling hit; over is over, stop walking
		}
		return nil
	})

	var checks []repoCheck
	checks = append(checks, sizeCheck(totalBytes, fileCount, over, limits))
	checks = append(checks, toolchainCheck(langManifest))
	checks = append(checks, egressCheck(dir, ecos, eg))
	if policyActive {
		checks = append(checks, diffPolicyCheck(blocked, second))
	}
	return checks
}

func sizeCheck(totalBytes int64, fileCount int, over bool, limits stageLimits) repoCheck {
	if over {
		return repoCheck{
			Label: "size",
			Detail: fmt.Sprintf("repo is >= %s / %d files, exceeds the %s / %d-file stage cap — the stage guard will kill the task",
				fmtBytes(totalBytes), fileCount, fmtBytes(limits.MaxBytes), limits.MaxFiles),
		}
	}
	return repoCheck{
		Label: "size", OK: true,
		Detail: fmt.Sprintf("%s / %d files (stage cap %s / %d files)",
			fmtBytes(totalBytes), fileCount, fmtBytes(limits.MaxBytes), limits.MaxFiles),
	}
}

func toolchainCheck(langManifest map[string]string) repoCheck {
	langs := make([]string, 0, len(langManifest))
	for l := range langManifest {
		langs = append(langs, l)
	}
	sort.Strings(langs)

	var shipped, missing []string
	for _, l := range langs {
		if v, ok := imageShips(l); ok {
			shipped = append(shipped, fmt.Sprintf("%s %s (%s)", l, v, langManifest[l]))
		} else {
			missing = append(missing, fmt.Sprintf("%s (%s)", l, langManifest[l]))
		}
	}
	switch {
	case len(missing) > 0:
		return repoCheck{
			Label: "toolchain", OK: true, Warn: true,
			Detail: fmt.Sprintf("repo uses %s; the sandbox image ships node/python/go only — the agent cannot build it without a custom sandbox_image",
				strings.Join(missing, ", ")),
		}
	case len(shipped) > 0:
		return repoCheck{Label: "toolchain", OK: true, Detail: "image ships " + strings.Join(shipped, ", ")}
	default:
		return repoCheck{Label: "toolchain", OK: true, Detail: "no recognized language manifests"}
	}
}

func egressCheck(dir string, ecos map[string]bool, eg egress.Config) repoCheck {
	allowed := map[string]bool{}
	for _, d := range eg.Default.Domains {
		allowed[d.Host] = true
	}

	ecoNames := make([]string, 0, len(ecos))
	for e := range ecos {
		ecoNames = append(ecoNames, e)
	}
	sort.Strings(ecoNames)

	var missing []string
	seen := map[string]bool{}
	for _, eco := range ecoNames {
		for _, h := range ecoRegistryHosts[eco] {
			if !allowed[h] && !seen[h] {
				seen[h] = true
				missing = append(missing, fmt.Sprintf("%s (%s default registry)", h, eco))
			}
		}
	}
	for _, cr := range customRegistryHosts(dir) {
		if !allowed[cr.host] && !seen[cr.host] {
			seen[cr.host] = true
			missing = append(missing, fmt.Sprintf("%s (custom registry in %s)", cr.host, cr.file))
		}
	}

	if len(missing) > 0 {
		return repoCheck{
			Label: "egress", OK: true, Warn: true,
			Detail: fmt.Sprintf("not in the egress allowlist: %s — submit with --egress-extra <host>:443 or add to egress.yaml",
				strings.Join(missing, ", ")),
		}
	}
	if len(ecoNames) == 0 {
		return repoCheck{Label: "egress", OK: true, Detail: "no package ecosystems detected"}
	}
	return repoCheck{Label: "egress", OK: true, Detail: "registry hosts for " + strings.Join(ecoNames, ", ") + " are allowlisted"}
}

func diffPolicyCheck(blocked, second int) repoCheck {
	switch {
	case blocked > 0:
		d := fmt.Sprintf("%d file(s) match diff_policy.blocked_paths — every task touching them fails as policy_blocked", blocked)
		if second > 0 {
			d += fmt.Sprintf("; %d more are second-look", second)
		}
		return repoCheck{Label: "diff_policy", Detail: d}
	case second > 0:
		return repoCheck{
			Label: "diff_policy", OK: true, Warn: true,
			Detail: fmt.Sprintf("%d file(s) match diff_policy.second_look_paths — approving a diff touching them needs acknowledgment", second),
		}
	default:
		return repoCheck{Label: "diff_policy", OK: true, Detail: "no repo files match blocked or second-look paths"}
	}
}

// maxRegistryConfigBytes caps how much of a registry config file is read; the
// registry= / index-url= lines of any sane .npmrc/pip.conf sit well within it.
const maxRegistryConfigBytes = 64 << 10

// registryConfigFiles are the top-level files scanned for a custom registry
// host. Read byte-capped; only url.Parse-extracted hostnames ever leave this
// scan — these files commonly hold auth tokens that must never be echoed.
var registryConfigFiles = []string{".npmrc", ".yarnrc", "Cargo.toml", "pip.conf"}

type customRegistry struct{ host, file string }

// customRegistryHosts extracts custom registry hostnames from the repo's
// package-manager config files: lines that assign a registry or index-url to
// an http(s) URL. Returns hostnames only, never tokens or raw lines. Opened
// with O_NOFOLLOW to match the walk's never-follow-symlink invariant — a repo
// must not be able to point .npmrc at an out-of-repo file and have doctor
// read it.
func customRegistryHosts(dir string) []customRegistry {
	var out []customRegistry
	seen := map[string]bool{}
	for _, name := range registryConfigFiles {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(io.LimitReader(f, maxRegistryConfigBytes))
		for sc.Scan() {
			if h := registryHostFromLine(sc.Text()); h != "" && !seen[h] {
				seen[h] = true
				out = append(out, customRegistry{host: h, file: name})
			}
		}
		f.Close()
	}
	return out
}

// registryHostFromLine returns the hostname of an http(s) URL on a line that
// actually ASSIGNS a registry: `registry=` / `@scope:registry=` (.npmrc),
// `index-url=` (pip.conf), `registry = "…"` / `index = "…"` (Cargo.toml
// [source.*]/[registries.*] tables), or yarn classic's `registry "…"`.
// Comments and lines that merely mention the word "registry" near a URL —
// including auth-token lines — map to "". Only the url.Parse hostname is
// returned, never the raw line.
func registryHostFromLine(line string) string {
	l := strings.TrimSpace(line)
	// Comment lines (npmrc/pip.conf use #/;) and npmrc //host/:_authToken
	// scope lines are never assignments.
	if strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") || strings.HasPrefix(l, "//") {
		return ""
	}
	var raw string
	if key, rest, ok := strings.Cut(l, "="); ok {
		key = strings.ToLower(strings.TrimSpace(key))
		switch {
		case key == "registry", key == "index-url", key == "index":
		case strings.HasSuffix(key, ":registry"): // npm scoped: @scope:registry=
		default:
			return ""
		}
		raw = rest
	} else if fields := strings.Fields(l); len(fields) == 2 && strings.ToLower(fields[0]) == "registry" {
		raw = fields[1] // yarn classic: registry "https://…"
	} else {
		return ""
	}
	raw = strings.TrimLeft(strings.TrimSpace(raw), `"'`)
	if lower := strings.ToLower(raw); !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		return ""
	}
	if k := strings.IndexAny(raw, " \t\"'"); k >= 0 {
		raw = raw[:k]
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// fmtBytes renders a byte count with a binary-unit suffix, one decimal.
func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
