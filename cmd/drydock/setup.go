package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"drydock/internal/config"
	"drydock/internal/netfw"
)

// firstRunBeforeInit reports whether ~/.drydock/config.yaml is absent. It MUST
// be called before runInit(), which seeds that file — otherwise the answer is
// always false and the wizard never fires on a genuine first run.
func firstRunBeforeInit(cfgPath string) bool {
	_, err := os.Stat(cfgPath)
	return os.IsNotExist(err)
}

// runSetup is the one-shot first-run path: install the Homebrew prerequisites
// drydock needs (Apple `container`, squid), then run init, then init prints
// what to do next. `drydock init` only *checks* these and exits if they're
// missing; setup closes that gap so a fresh install goes from zero to a built
// sandbox in one command instead of a README scavenger hunt.
//
// `--yes` installs without prompting (CI / unattended). Without a TTY and
// without `--yes`, setup prints the install command and stops rather than
// silently modifying the system.
func runSetup(args []string) {
	yes := false
	reconfigure := false
	for _, a := range args {
		switch a {
		case "-y", "--yes":
			yes = true
		case "--reconfigure":
			reconfigure = true
		case "-h", "--help", "help":
			fmt.Printf("drydock setup — %s\n", subHelp["setup"])
			os.Exit(0)
		}
	}

	// Sample firstRun BEFORE runInit() — runInit seeds config.yaml via
	// ensureUserConfig/seedConfig, so os.Stat would always find the file
	// afterwards and firstRun would always be false. See firstRunBeforeInit.
	cfgPath := config.DefaultPath()
	firstRun := firstRunBeforeInit(cfgPath)

	fmt.Println("drydock setup — install prerequisites, then first-time setup")
	fmt.Println()
	checkPlatform()

	// Homebrew is how we install the two prereqs; we can't bootstrap it.
	if _, err := exec.LookPath("brew"); err != nil {
		step("Homebrew", false, "not found — install from https://brew.sh, then re-run `drydock setup`")
		os.Exit(1)
	}

	// 1. Apple container CLI (cask).
	if _, err := exec.LookPath("container"); err == nil {
		step("Apple container CLI", true, "already installed")
	} else if !ensurePrereq(yes, "container", "Apple container runtime", "brew", "install", "--cask", "container") {
		os.Exit(1)
	}

	// 2. Userspace squid (registry egress proxy). Discovered the same way
	// brokerd discovers it, so a brew-sbin install still counts.
	if _, err := netfw.FindSquid(); err == nil {
		step("userspace squid proxy", true, "already installed")
	} else if !ensurePrereq(yes, "squid", "userspace squid proxy", "brew", "install", "squid") {
		os.Exit(1)
	}

	// 3. Hand off to the existing first-time setup (container system, network,
	// images, ~/.drydock seed). init re-verifies the prereqs we just installed
	// and prints the "next:" steps.
	fmt.Println()
	fmt.Println("prerequisites ready — running drydock init…")
	fmt.Println()
	runInit()

	// First-run / explicit reconfigure → interactive wizard. Non-TTY or an
	// existing config without --reconfigure keeps init's static seed + "next:".
	// firstRun was sampled above, before runInit seeded the config file.
	if tty && stdinIsTTY() && (firstRun || reconfigure) {
		fmt.Println()
		fmt.Println("── configure ───────────────────────────────")
		runWizard(&wizardDeps{
			in:              os.Stdin,
			out:             os.Stdout,
			bootstrapClaude: bootstrapClaudeCred,
			bootstrapCodex:  bootstrapCodexCred,
			configPath:      cfgPath,
		})
	}
}

// ensurePrereq installs one Homebrew package, prompting first unless yes is
// set. Returns false if the install was declined, skipped, or failed.
func ensurePrereq(yes bool, name, label string, installCmd ...string) bool {
	cmdStr := strings.Join(installCmd, " ")
	if !yes {
		if !stdinIsTTY() {
			step(label, false, "missing — run: "+cmdStr+"  (or re-run `drydock setup --yes`)")
			return false
		}
		fmt.Printf("  install %s? runs `%s` [y/N] ", name, cmdStr)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if l := strings.ToLower(strings.TrimSpace(line)); l != "y" && l != "yes" {
			step(label, false, "skipped — run `"+cmdStr+"` yourself, then re-run setup")
			return false
		}
	}
	fmt.Printf("  · %s …\n", cmdStr)
	c := exec.Command(installCmd[0], installCmd[1:]...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		step(label, false, "install failed: "+err.Error())
		return false
	}
	step(label, true, "installed")
	return true
}

// stdinIsTTY reports whether stdin is an interactive terminal, so setup knows
// whether it can prompt. Mirrors the stdout `tty` check in init.go.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}
