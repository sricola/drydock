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

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		runInit()
	case "start":
		runStart()
	case "submit":
		runSubmit(os.Args[2:])
	case "status":
		runStatus()
	case "tasks":
		runTasks()
	case "logs":
		mustArgsRange(2, 3)
		follow := len(os.Args) == 4 && (os.Args[3] == "-f" || os.Args[3] == "--follow")
		runLogs(os.Args[2], follow)
	case "review":
		mustArgs(2)
		runReview(os.Args[2])
	case "kill":
		mustArgs(2)
		runKill(os.Args[2])
	case "pending":
		listPending()
	case "approve":
		mustArgs(2)
		signal("approve", os.Args[2])
	case "deny":
		mustArgs(2)
		signal("deny", os.Args[2])
	case "version", "--version", "-v":
		fmt.Println("drydock", version)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "drydock: unknown command %q\n\n", os.Args[1])
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
