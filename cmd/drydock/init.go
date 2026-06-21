package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"drydock/internal/config"
	"drydock/internal/netfw"
)

// tty is true if stdout looks like an interactive terminal. We emit ANSI
// colors only when true; piping to a log file otherwise produces "[32m" garbage.
var tty = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}()

// runInit walks through every prerequisite drydock needs and reports per-step
// status. Idempotent: re-running after a partial success picks up where the
// previous run left off. Designed for "I just cloned this — what now?".
func runInit() {
	fmt.Println("drydock init — first-time setup")
	fmt.Println()

	checkPlatform()
	checkBinary("container", "Apple container CLI", "Install: brew install --cask container (https://github.com/apple/container)")
	checkSquid()
	checkBinary("git", "git", "Install: xcode-select --install")

	ensureContainerSystem()
	ensureNetwork()
	ensureImage()
	ensureUserConfig()
	nudgeStaleSandboxImage()

	fmt.Println()
	fmt.Println("ready. next:")
	fmt.Println("  1. export ANTHROPIC_API_KEY=sk-ant-...")
	fmt.Println("  2. edit ~/.drydock/{config,egress}.yaml if you want non-defaults")
	fmt.Println("  3. drydock start                       (look for `brokerd listening on unix://...`)")
	fmt.Println("  4. drydock status / pending / approve  (in another shell)")
}

// ensureUserConfig creates ~/.drydock/ at 0700 if missing and seeds
// config.yaml and egress.yaml from defaults / the share-dir template. Never
// overwrites an existing file — operator edits are sacred.
func ensureUserConfig() {
	dir := config.Dir()
	if dir == "" {
		step("~/.drydock/", false, "could not resolve home directory")
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		step("~/.drydock/", false, err.Error())
		return
	}

	cfgPath := config.DefaultPath()
	if _, err := os.Stat(cfgPath); err == nil {
		step("~/.drydock/config.yaml", true, "exists (your edits preserved)")
	} else {
		seedConfig(cfgPath)
	}

	egPath := config.EgressPath()
	if _, err := os.Stat(egPath); err == nil {
		step("~/.drydock/egress.yaml", true, "exists (your edits preserved)")
		nudgeEgressRecommendations(egPath)
		return
	}
	// Seed from the share-dir template (or a sane default if not present).
	tmpl, err := findShareEgressTemplate()
	if err != nil {
		// Fall back to a minimal default so brokerd can boot even without
		// the share template.
		_ = os.WriteFile(egPath, []byte(defaultEgressYAML), 0o644)
		step("~/.drydock/egress.yaml", true, "seeded with built-in default")
		return
	}
	b, rerr := os.ReadFile(tmpl)
	if rerr != nil {
		step("~/.drydock/egress.yaml", false, rerr.Error())
		return
	}
	if werr := os.WriteFile(egPath, b, 0o644); werr != nil {
		step("~/.drydock/egress.yaml", false, werr.Error())
		return
	}
	step("~/.drydock/egress.yaml", true, "seeded from "+tmpl)
}

// seedConfig writes ~/.drydock/config.yaml from the share-dir template
// (brew/make install) when available, falling back to the embedded
// SeedTemplate Go const when not. Mirrors the egress.yaml flow below so
// brew-installed users get the commented template and source-only users
// still get a working file.
func seedConfig(cfgPath string) {
	if tmpl, err := findShareConfigTemplate(); err == nil {
		if b, rerr := os.ReadFile(tmpl); rerr == nil {
			if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err == nil {
				if werr := os.WriteFile(cfgPath, b, 0o644); werr == nil {
					step("~/.drydock/config.yaml", true, "seeded from "+tmpl)
					return
				}
			}
		}
	}
	if werr := config.WriteSeed(cfgPath); werr != nil {
		step("~/.drydock/config.yaml", false, werr.Error())
	} else {
		step("~/.drydock/config.yaml", true, "seeded with built-in default")
	}
}

// findShareConfigTemplate locates config/config.yaml in the share-dir
// layout (brew, make install) or the cloned-repo dev case. Mirrors
// findShareEgressTemplate.
func findShareConfigTemplate() (string, error) {
	return findShareFile("config.yaml")
}

// findShareEgressTemplate locates config/egress.yaml in the share-dir layout
// (brew, make install) or the cloned-repo dev case. Mirrors findImageDir.
func findShareEgressTemplate() (string, error) {
	return findShareFile("egress.yaml")
}

// findShareFile is the shared probe used by both share-dir lookups. Search
// order matches findImageDir: alongside the drydock binary, $HOMEBREW_PREFIX,
// then the cloned-repo dev case.
func findShareFile(name string) (string, error) {
	candidates := []string{}
	if self, err := os.Executable(); err == nil {
		root := filepath.Dir(filepath.Dir(self))
		candidates = append(candidates,
			filepath.Join(root, "share", "drydock", "config", name))
	}
	if hb := os.Getenv("HOMEBREW_PREFIX"); hb != "" {
		candidates = append(candidates,
			filepath.Join(hb, "share", "drydock", "config", name))
	}
	candidates = append(candidates, filepath.Join("config", name))
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("share-dir %s not found", name)
}

// defaultEgressYAML is the absolute fallback — used only if the share-dir
// template is unreachable. Must stay in sync with config/egress.yaml.
const defaultEgressYAML = `version: 1
default:
  allow_dns: true
  domains:
    - { host: api.anthropic.com,      ports: [443] }
    - { host: api.openai.com,         ports: [443] }
    # JavaScript
    - { host: registry.npmjs.org,     ports: [443] }
    # Python
    - { host: pypi.org,               ports: [443] }
    - { host: files.pythonhosted.org, ports: [443] }
    # Go module ecosystem — proxy.golang.org is the module cache; sum.golang.org
    # is the checksum DB (Go refuses to fetch without it unless GOSUMDB=off).
    - { host: proxy.golang.org,       ports: [443] }
    - { host: sum.golang.org,         ports: [443] }
  cidrs: []
per_task_widening:
  requires_approval: true
`

// recommendedEgressHosts is the allowlist a fresh install would receive.
// When an existing operator config (~/.drydock/egress.yaml) lacks any of
// these, init prints a one-shot nudge with the exact YAML to add. We do
// not auto-edit — operator edits to that file are sacred. Add new shipping
// entries here in the same PR that touches config/egress.yaml.
var recommendedEgressHosts = []string{
	"proxy.golang.org",
	"sum.golang.org",
}

// nudgeEgressRecommendations checks the existing egress.yaml for the hosts
// in recommendedEgressHosts. For each one missing, it prints a single
// hint block telling the operator what to add and where. Empty list ->
// silent. Errors reading the file -> silent (we already printed "exists").
func nudgeEgressRecommendations(path string) {
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var missing []string
	for _, h := range recommendedEgressHosts {
		// Match the bare hostname; YAML quoting variants don't trip this
		// because the host appears verbatim in either { host: x } or
		// indented map style.
		if !strings.Contains(string(body), h) {
			missing = append(missing, h)
		}
	}
	if len(missing) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("  ! ~/.drydock/egress.yaml is missing recommended entries.")
	fmt.Println("    Add to default.domains: in", path, "—")
	for _, h := range missing {
		fmt.Printf("        - { host: %s, ports: [443] }\n", h)
	}
	fmt.Println("    (drydock won't edit your egress.yaml; copy these in by hand.)")
}

// step prints a one-line status. ok=true → "✓"; ok=false → "✗".
func step(label string, ok bool, detail string) {
	var mark string
	switch {
	case ok && tty:
		mark = "\033[32m✓\033[0m"
	case !ok && tty:
		mark = "\033[31m✗\033[0m"
	case ok:
		mark = "ok "
	default:
		mark = "FAIL"
	}
	fmt.Printf("  %s %-32s %s\n", mark, label, detail)
}

// checkPlatform fails loudly if drydock is run on a host where Apple's
// `container` runtime can't work — non-darwin, or non-arm64. The Homebrew
// formula already declares these constraints, but source builds skip that
// path. Without this check, the failure surfaces 5 steps later as a cryptic
// `container build` error or vmnet bind failure.
func checkPlatform() {
	if runtime.GOOS != "darwin" {
		step("platform", false, "drydock requires macOS — Apple container is darwin-only")
		os.Exit(1)
	}
	if runtime.GOARCH != "arm64" {
		step("platform", false, "drydock requires Apple silicon (arm64) — running on "+runtime.GOARCH)
		os.Exit(1)
	}
	// sw_vers ProductVersion → e.g. "26.0.0". `container` needs macOS 26+.
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		// sw_vers always exists on macOS; if we can't run it something is
		// very off, but don't block on that — let the downstream check fail.
		step("platform", true, runtime.GOOS+"/"+runtime.GOARCH+" (macOS version unknown)")
		return
	}
	ver := strings.TrimSpace(string(out))
	majorStr := strings.SplitN(ver, ".", 2)[0]
	major, err := strconv.Atoi(majorStr)
	if err == nil && major < 26 {
		step("platform", false, "macOS "+ver+" — Apple container requires macOS 26 (Tahoe) or newer")
		os.Exit(1)
	}
	step("platform", true, "macOS "+ver+" "+runtime.GOARCH)
}

func checkBinary(name, label, fix string) {
	p, err := exec.LookPath(name)
	if err != nil {
		step(label, false, "not found on PATH — "+fix)
		os.Exit(1)
	}
	step(label, true, p)
}

// checkSquid mirrors netfw.FindSquid's discovery so the init check matches
// brokerd's runtime discovery. Homebrew installs squid into a sbin path that
// often isn't on a default PATH; brokerd handles that, init should too.
func checkSquid() {
	p, err := netfw.FindSquid()
	if err != nil {
		step("userspace squid proxy", false, "not found — Install: brew install squid")
		os.Exit(1)
	}
	step("userspace squid proxy", true, p)
}

func ensureContainerSystem() {
	// `container --version` only requires the CLI; `container network ls`
	// requires the system service. Try the latter to detect "service down".
	if out, err := exec.Command("container", "network", "ls").CombinedOutput(); err != nil {
		if strings.Contains(string(out), "XPC connection") || strings.Contains(string(out), "system service") {
			fmt.Println("  · container system not running — starting (may install kernel on first run)…")
			cmd := exec.Command("container", "system", "start", "--enable-kernel-install")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				step("container system", false, err.Error())
				os.Exit(1)
			}
			step("container system", true, "started")
			return
		}
		step("container system", false, strings.TrimSpace(string(out)))
		os.Exit(1)
	}
	step("container system", true, "already running")
}

func ensureNetwork() {
	out, err := exec.Command("container", "network", "ls").CombinedOutput()
	if err != nil {
		step("network", false, strings.TrimSpace(string(out)))
		os.Exit(1)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "drydock-egress") {
			step("network drydock-egress", true, "exists (192.168.66.0/24)")
			return
		}
	}
	if out, err := exec.Command("container", "network", "create",
		"--subnet", "192.168.66.0/24", "drydock-egress").CombinedOutput(); err != nil {
		step("network drydock-egress", false, strings.TrimSpace(string(out)))
		os.Exit(1)
	}
	step("network drydock-egress", true, "created (192.168.66.0/24)")
}

// defaultSandboxImage is the sandbox image init builds and tags. Keep in sync
// with the ensureImage call below; the nudge compares the operator's configured
// sandbox_image against it.
const defaultSandboxImage = "drydock-sandbox:latest"

func ensureImage() {
	ensureNamedImage("drydock-sandbox", "image", "first build can take a few minutes")
	ensureNamedImage("drydock-anchor", "image/anchor", "minimal — usually quick")
}

// nudgeStaleSandboxImage warns when the operator's configured sandbox_image
// differs from the image init just built. The usual cause is a config seeded
// before the v0.1.5 claude-sandbox -> drydock-sandbox rename: init builds the
// new image, but tasks keep pointing at the stale name (and lose Codex). We
// never edit the operator's config — just point at the one-line fix.
func nudgeStaleSandboxImage() {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return // config load errors surface on `drydock start`, not here
	}
	if warn, msg := sandboxImageNudge(cfg.SandboxImage, defaultSandboxImage); warn {
		fmt.Println()
		fmt.Println(msg)
	}
}

// sandboxImageNudge returns the warning to print when the configured sandbox
// image isn't the one init builds. Empty (use-default) and an exact match are
// silent. A pure helper so the decision is testable without a real config.
func sandboxImageNudge(configured, built string) (bool, string) {
	if configured == "" || configured == built {
		return false, ""
	}
	return true, "  ! ~/.drydock/config.yaml sets sandbox_image: " + configured + "\n" +
		"    but init built " + built + " — tasks will use " + configured + ".\n" +
		"    If that's a pre-v0.1.5 image (e.g. claude-sandbox), set it to " + built + "."
}

func ensureNamedImage(name, subdir, note string) {
	label := "image " + name + ":latest"
	listed, _ := exec.Command("container", "image", "list").CombinedOutput()
	have := strings.Contains(string(listed), name)

	// Existing image is necessary but not sufficient: a layer cache from before
	// the macagent→drydock rename can leave a stale entrypoint.sh that reads
	// MACAGENT_GW_IP instead of DRYDOCK_GW_IP, and every task fails to boot.
	// For the sandbox image, peek inside and force a no-cache rebuild on drift.
	stale := false
	if have && name == "drydock-sandbox" {
		got, err := exec.Command("container", "run", "--rm", "--entrypoint", "/bin/cat",
			name+":latest", "/usr/local/bin/entrypoint.sh").CombinedOutput()
		if err == nil && !strings.Contains(string(got), "DRYDOCK_GW_IP") {
			stale = true
		}
	}
	if have && !stale {
		step(label, true, "exists")
		return
	}

	ctxDir, err := findImageDir(subdir)
	if err != nil {
		step(label, false, "Dockerfile not found: "+err.Error())
		fmt.Printf("    → set DRYDOCK_IMAGE_DIR to drydock's image/ dir, or:\n")
		fmt.Printf("    → container build -t %s:latest <path-to-drydock>/%s\n", name, subdir)
		os.Exit(1)
	}
	args := []string{"build", "-t", name + ":latest", ctxDir}
	noteOut := note
	if stale {
		// Drop the old image first so layer caching doesn't re-pull the stale
		// entrypoint.sh, then force a no-cache rebuild.
		_ = exec.Command("container", "image", "delete", name+":latest").Run()
		args = []string{"build", "--no-cache", "-t", name + ":latest", ctxDir}
		noteOut = "stale entrypoint detected — rebuilding from scratch"
	}
	fmt.Printf("  · building %s:latest from %s (%s)…\n", name, ctxDir, noteOut)
	cmd := exec.Command("container", args...)
	// Tee the build output to a buffer so we can recognise known failure modes
	// (e.g. Apple container shipping an empty build context) and add guidance.
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	if err := cmd.Run(); err != nil {
		step(label, false, err.Error())
		if hint := imageBuildHint(buf.String()); hint != "" {
			fmt.Printf("    → %s\n", hint)
		}
		os.Exit(1)
	}
	step(label, true, "built")
}

// imageBuildHint returns operator guidance when a `container build` failure
// matches the signature of Apple container's empty-build-context bug: the local
// files never reach the builder ("transferring context: 2B"), so a COPY fails
// on files that are present on disk. Empty when the output doesn't match, so the
// caller falls back to the raw error for genuine Dockerfile/registry problems.
func imageBuildHint(buildOutput string) string {
	o := strings.ToLower(buildOutput)
	emptyContext := strings.Contains(o, "transferring context: 2b")
	copyMiss := strings.Contains(o, "failed to compute cache key") ||
		(strings.Contains(o, "calculate checksum") && strings.Contains(o, "not found"))
	if !emptyContext && !copyMiss {
		return ""
	}
	return strings.Join([]string{
		"the build context didn't reach the builder — a known Apple `container` runtime issue, not a drydock problem.",
		"      the COPY'd files exist on disk but `container build` shipped an empty context. Try, in order:",
		"        1. container system stop && container system start",
		"        2. reboot (clears the builder VM's context sharing)",
		"        3. check/downgrade `container` if it was recently upgraded",
		"      an already-built image still works; you only need this to (re)build it.",
	}, "\n")
}

// findImageDir locates the Dockerfile build context. Search order:
//  1. DRYDOCK_IMAGE_DIR/<subdir> (explicit operator override)
//  2. ./<subdir> (running from drydock repo root)
//  3. <drydock binary>/../share/drydock/<subdir> (Homebrew & make-install
//     when PREFIX/share is set up)
//  4. $HOMEBREW_PREFIX/share/drydock/<subdir>
//  5. ~/.local/share/drydock/<subdir>
func findImageDir(subdir string) (string, error) {
	candidates := []string{}
	if env := os.Getenv("DRYDOCK_IMAGE_DIR"); env != "" {
		candidates = append(candidates, filepath.Join(env, strings.TrimPrefix(subdir, "image/")))
		candidates = append(candidates, filepath.Join(env, subdir))
	}
	candidates = append(candidates, subdir)
	if self, err := os.Executable(); err == nil {
		// drydock at $PREFIX/bin/drydock -> $PREFIX/share/drydock/<subdir>
		root := filepath.Dir(filepath.Dir(self))
		candidates = append(candidates,
			filepath.Join(root, "share", "drydock", subdir),
		)
	}
	if hb := os.Getenv("HOMEBREW_PREFIX"); hb != "" {
		candidates = append(candidates, filepath.Join(hb, "share", "drydock", subdir))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "share", "drydock", subdir))
	}

	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "Dockerfile")); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("searched: %s", strings.Join(candidates, ", "))
}
