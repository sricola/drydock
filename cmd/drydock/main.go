// drydock is the operator CLI: first-time setup (`init`), running brokerd
// (`start`), and reviewing pending tasks (`pending`/`approve`/`deny`).
package main

import (
	"flag"
	"fmt"
	"os"
)

func usage() {
	fmt.Fprint(os.Stderr, `drydock — local containment for autonomous coding agents

Setup:
  drydock setup                  one command: install prerequisites (container, squid) + init
  drydock init                   one-time setup: container service, network, image, smoke
  drydock start                  run brokerd in the foreground (expects ANTHROPIC_API_KEY and/or OPENAI_API_KEY)
  drydock daemon install|uninstall|status   run brokerd unattended via launchd (starts at login, restarts on crash)
  drydock status                 brokerd up?, pending count, recent tasks
  drydock doctor                 smoke-test the sandbox setup (no API spend)
  drydock redteam                run live containment attacks on your sandbox (no API spend)
  drydock auth claude|codex      bootstrap Claude or ChatGPT/Codex subscription credentials for brokerd
  drydock ui [--port N] [--open] [--no-token]   local web UI (loopback, token-gated)

Tasks:
  drydock submit <flags>         POST a new task; blocks until approval/completion
  drydock tasks                  list recent runs (id, age, duration, cost, outcome)
  drydock logs    <id> [-f]      print (or follow) the task's stream-json audit log
  drydock review  <id>           open the diff in $PAGER, then prompt y/N
  drydock retry   <id>           re-run a prior task from its recorded invocation
  drydock kill    <id>           tear down the VM and deny if pending (alias: cancel)
  drydock prune   <flags>        delete old audit artifacts (--older-than DUR [--keep-last N] [--yes])

Approvals:
  drydock pending                list task IDs awaiting approval
  drydock approve <id>           approve the pending push
  drydock deny    <id>           deny the pending push (diff returned but not pushed)

Other:
  drydock version                print drydock version
  drydock help                   this message

Connection (approvals & status):
  Defaults to a per-user Unix socket ($TMPDIR/drydock-$UID/drydock.sock).
  Override with BROKER_SOCKET=path or BROKER_ADDR=host:port.
Environment:
  AUDIT_ROOT          override audit dir (default ~/.drydock/audit)
  PAGER               viewer used by 'review' (default 'less -R')
  DRYDOCK_NO_NOTIFY=1 silence brokerd's macOS notifications
`)
}

// version is set at build time via -ldflags. Falls back to "dev" when unset.
var version = "dev"

// subHelp maps each subcommand to its one-line description shown by
// `drydock <cmd> --help`. Centralizing this means a new subcommand can't
// silently ship without help text — the dispatcher routes through subHelp
// before consuming any positional args, so `drydock approve --help` can
// never accidentally approve a task literally named "--help".
var subHelp = map[string]string{
	"setup":   "first run: install prerequisites (container, squid), then the setup wizard. --reconfigure re-runs the wizard; --yes to skip install prompts.",
	"init":    "first-time setup: container service, network, sandbox image, ~/.drydock seed. Idempotent.",
	"start":   "run brokerd in the foreground. Requires ANTHROPIC_API_KEY and/or OPENAI_API_KEY in env. ^C to stop.",
	"daemon":  "install|uninstall|status — manage the brokerd LaunchAgent (login start, crash restart). Credentials must be host-side (api-keys.env / oauth files).",
	"status":  "brokerd up?, in-flight stage breakdown, recent task counts.",
	"tasks":   "list recent runs: id, age, duration, cost, outcome.",
	"logs":    "<id> [-f] — print the task's stream-json audit log; -f to follow.",
	"review":  "<id> — open the diff in $PAGER, prompt y/N to approve or deny.",
	"kill":    "<id> — tear down the VM and deny if pending.",
	"cancel":  "<id> — alias for kill: tear down the VM and deny if pending.",
	"retry":   "<id> — re-run a prior task from its recorded invocation (repo + prompt + flags; re-enters the approval gate).",
	"prune":   "delete old per-task audit artifacts; --older-than DUR [--keep-last N] [--yes]. Dry-run unless --yes.",
	"pending": "list task IDs awaiting approval (egress + diff gates both shown).",
	"approve": "<id> — approve the pending push for <id>.",
	"deny":    "<id> — deny the pending push (diff captured, not pushed).",
	"doctor":  "smoke-test the sandbox setup: image freshness, VM boot, egress pin. No API spend.",
	"redteam": "run live containment attacks (A1 key-exfil, A2 egress, A7 ephemerality) against your sandbox. No API spend.",
	"auth":    "auth claude|codex [--status] — bootstrap Claude or ChatGPT/Codex subscription creds into ~/.drydock/.",
	"submit":  "POST a new task; see `drydock submit -h` for the full flag list.",
	"ui":      "--port N --open --no-token — run the loopback web UI (token-gated, 127.0.0.1 only).",
	"version": "print drydock version.",
}

// consumeHelpFlag prints per-subcommand help and exits 0 when args[0] is
// -h / --help / help. Must be called before any positional consumption.
func consumeHelpFlag(cmd string, args []string) {
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Printf("drydock %s — %s\n", cmd, subHelp[cmd])
		os.Exit(0)
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	subArgs := os.Args[2:]
	switch cmd {
	case "setup":
		runSetup(subArgs)
	case "init":
		consumeHelpFlag(cmd, subArgs)
		runInit()
	case "start":
		consumeHelpFlag(cmd, subArgs)
		runStart()
	case "daemon":
		consumeHelpFlag(cmd, subArgs)
		runDaemon(subArgs)
	case "submit":
		// `submit` has its own flag.FlagSet which handles -h/--help.
		runSubmit(subArgs)
	case "status":
		consumeHelpFlag(cmd, subArgs)
		runStatus()
	case "tasks":
		consumeHelpFlag(cmd, subArgs)
		runTasks()
	case "logs":
		consumeHelpFlag(cmd, subArgs)
		fs := flag.NewFlagSet("logs", flag.ExitOnError)
		follow := fs.Bool("f", false, "follow the log as brokerd appends (like tail -f)")
		fs.BoolVar(follow, "follow", false, "alias for -f")
		_ = fs.Parse(subArgs)
		if fs.NArg() != 1 {
			die("usage: drydock logs <id> [-f]")
		}
		runLogs(fs.Arg(0), *follow)
	case "retry":
		consumeHelpFlag(cmd, subArgs)
		mustArgs(2)
		runRetry(os.Args[2])
	case "review":
		consumeHelpFlag(cmd, subArgs)
		mustArgs(2)
		runReview(os.Args[2])
	case "kill", "cancel": // cancel is an alias — the verb users reach for first
		consumeHelpFlag(cmd, subArgs)
		mustArgs(2)
		runKill(os.Args[2])
	case "prune":
		// `prune` has its own flag.FlagSet which handles -h/--help.
		runPrune(subArgs)
	case "pending":
		consumeHelpFlag(cmd, subArgs)
		listPending()
	case "approve":
		consumeHelpFlag(cmd, subArgs)
		mustArgs(2)
		signal("approve", os.Args[2])
	case "deny":
		consumeHelpFlag(cmd, subArgs)
		mustArgs(2)
		signal("deny", os.Args[2])
	case "doctor":
		consumeHelpFlag(cmd, subArgs)
		runDoctor()
	case "redteam":
		consumeHelpFlag(cmd, subArgs)
		runRedteam()
	case "auth":
		consumeHelpFlag(cmd, subArgs)
		runAuth(subArgs)
	case "ui":
		consumeHelpFlag(cmd, subArgs)
		runUI(subArgs)
	case "version", "--version", "-v":
		fmt.Println("drydock", version)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "drydock: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func mustArgs(want int) {
	if len(os.Args) != want+1 {
		usage()
		os.Exit(2)
	}
}
