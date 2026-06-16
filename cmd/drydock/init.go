package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

	checkBinary("container", "Apple container CLI", "Install: brew install --cask container (https://github.com/apple/container)")
	checkSquid()
	checkBinary("git", "git", "Install: xcode-select --install")

	ensureContainerSystem()
	ensureNetwork()
	ensureImage()

	fmt.Println()
	fmt.Println("ready. next:")
	fmt.Println("  1. export ANTHROPIC_API_KEY=sk-ant-...")
	fmt.Println("  2. drydock start    (look for `brokerd listening on unix://...` in the boot log for the socket path)")
	fmt.Println("  3. in another shell: drydock status / drydock pending / drydock approve <id>")
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

func ensureImage() {
	ensureNamedImage("claude-sandbox", "image", "first build can take a few minutes")
	ensureNamedImage("drydock-anchor", "image/anchor", "minimal — usually quick")
}

func ensureNamedImage(name, subdir, note string) {
	label := "image " + name + ":latest"
	out, _ := exec.Command("container", "image", "list").CombinedOutput()
	if strings.Contains(string(out), name) {
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
	fmt.Printf("  · building %s:latest from %s (%s)…\n", name, ctxDir, note)
	cmd := exec.Command("container", "build", "-t", name+":latest", ctxDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		step(label, false, err.Error())
		os.Exit(1)
	}
	step(label, true, "built")
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
