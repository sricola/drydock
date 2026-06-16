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

Approvals:
  drydock pending                list task IDs awaiting approval
  drydock approve <task-id>      approve the pending push
  drydock deny    <task-id>      deny the pending push (diff returned but not pushed)

Other:
  drydock version                print drydock version
  drydock help                   this message

Connection (approvals):
  Defaults to unix:///tmp/drydock.sock. Override with BROKER_ADDR=host:port.
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
