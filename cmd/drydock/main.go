// drydock is the operator CLI: first-time setup (`init`), running brokerd
// (`start`), and reviewing pending tasks (`pending`/`approve`/`deny`).
package main

import (
	"fmt"
	"os"
)

func usage() {
	fmt.Fprint(os.Stderr, `drydock — local containment for autonomous coding agents

Setup:
  drydock init                   one-time setup: container service, network, image, smoke
  drydock start                  run brokerd in the foreground (expects ANTHROPIC_API_KEY)
  drydock status                 brokerd up?, pending count, recent tasks
  drydock doctor                 smoke-test the sandbox setup (no API spend)

Tasks:
  drydock submit <flags>         POST a new task; blocks until approval/completion
  drydock tasks                  list recent runs (id, age, duration, cost, outcome)
  drydock logs    <id> [-f]      print (or follow) the task's stream-json audit log
  drydock review  <id>           open the diff in $PAGER, then prompt y/N
  drydock kill    <id>           tear down the VM and deny if pending

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
  AUDIT_ROOT          override audit dir (default /tmp/broker/audit)
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
	"init":    "first-time setup: container service, network, sandbox image, ~/.drydock seed. Idempotent.",
	"start":   "run brokerd in the foreground. Requires ANTHROPIC_API_KEY in env. ^C to stop.",
	"status":  "brokerd up?, in-flight stage breakdown, recent task counts.",
	"tasks":   "list recent runs: id, age, duration, cost, outcome.",
	"logs":    "<id> [-f] — print the task's stream-json audit log; -f to follow.",
	"review":  "<id> — open the diff in $PAGER, prompt y/N to approve or deny.",
	"kill":    "<id> — tear down the VM and deny if pending.",
	"pending": "list task IDs awaiting approval (egress + diff gates both shown).",
	"approve": "<id> — approve the pending push for <id>.",
	"deny":    "<id> — deny the pending push (diff captured, not pushed).",
	"doctor":  "smoke-test the sandbox setup: image freshness, VM boot, egress pin. No API spend.",
	"submit":  "POST a new task; see `drydock submit -h` for the full flag list.",
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
	case "init":
		consumeHelpFlag(cmd, subArgs)
		runInit()
	case "start":
		consumeHelpFlag(cmd, subArgs)
		runStart()
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
		mustArgsRange(2, 3)
		follow := len(os.Args) == 4 && (os.Args[3] == "-f" || os.Args[3] == "--follow")
		runLogs(os.Args[2], follow)
	case "review":
		consumeHelpFlag(cmd, subArgs)
		mustArgs(2)
		runReview(os.Args[2])
	case "kill":
		consumeHelpFlag(cmd, subArgs)
		mustArgs(2)
		runKill(os.Args[2])
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

func mustArgsRange(min, max int) {
	n := len(os.Args) - 1
	if n < min || n > max {
		usage()
		os.Exit(2)
	}
}
