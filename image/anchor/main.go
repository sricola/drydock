//go:build linux

// Anchor is a Linux process that does nothing forever. brokerd keeps one
// container running on the drydock-egress network at all times so vmnet's
// gateway IP (192.168.66.1) stays bound — the gateway and squid bind to
// that IP and would otherwise come and go with the task VMs.
//
// This is intentionally tiny (FROM scratch + this static binary) so the
// anchor isn't a compromised-image foothold. The threat model used to
// note this; with the dedicated image, the anchor no longer shares attack
// surface with the agent VM.
//
// The `//go:build linux` tag keeps host-side `go build ./...` from
// compiling this for macOS; only the Dockerfile (which sets GOOS=linux)
// includes it.
package main

import (
	"os"
	"os/signal"
	"syscall"
)

func main() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
}
