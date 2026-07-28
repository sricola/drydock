package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// runCmd is the exec seam for shell-outs in the drydock client: container CLI
// invocations (kill, doctor's sandbox smoke checks) and, for doctor's push-
// credential check, `git config`. The default calls
// exec.Command(name, args...).CombinedOutput() so production behaviour is
// unchanged. Tests replace it with a fake.
var runCmd = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// runKill cancels a task. Preferred path is POST /admin/kill, which fires
// the brokerd-side context cancel — the container exits cleanly, the
// approval gate (if reached) unblocks with cancelled=true, and the
// `POST /tasks` request returns a structured cancellation response.
//
// Falls back to `container delete --force task-<id>` only when brokerd is
// unreachable or doesn't know the task — for example, the operator killed
// brokerd itself and the orphan container is still up.
//
// Returns the process exit code: 0 on a confirmed cancel/cleanup, 1 on any
// path that leaves the task unresolved (unknown id, brokerd error, or a
// failed cleanup attempt): matches the sibling approve/deny exit contract
// (see signal in client.go) rather than the old implicit 0 on every branch.
func runKill(id string) int {
	c, base := brokerClient()
	resp, err := c.Post(base+"/admin/kill/"+id, "", nil)
	if err == nil {
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusNoContent:
			fmt.Printf("task %s cancelled\n", id)
			return 0
		case http.StatusNotFound:
			// brokerd is up but doesn't track this task; try the
			// container CLI in case it's an orphan.
		default:
			fmt.Fprintf(os.Stderr, "drydock kill: brokerd returned %s\n", resp.Status)
			return 1
		}
	}
	if brokerdDown(err) {
		// The previous "no such task" path was misleading: it implied
		// brokerd had checked and didn't know the id, when really
		// brokerd wasn't even running. Be explicit before falling back
		// to the container CLI orphan-cleanup.
		fmt.Fprintln(os.Stderr, "drydock kill:", brokerDownHint)
		fmt.Fprintln(os.Stderr, "  attempting best-effort VM cleanup anyway…")
	}
	// Brokerd unreachable or 404 — best-effort VM cleanup.
	out, ferr := runCmd("container", "delete", "--force", "task-"+id)
	switch {
	case ferr == nil:
		fmt.Printf("task %s VM removed (brokerd didn't know about it)\n", id)
		return 0
	case isNoSuchContainer(string(out)):
		fmt.Fprintf(os.Stderr, "drydock kill: no such task %s\n", id)
		return 1
	default:
		fmt.Fprintf(os.Stderr, "drydock kill: container delete: %v\n%s", ferr, out)
		return 1
	}
}

// isNoSuchContainer recognises Apple container's "container not found"
// shape so we don't surface a noisy error when the task already exited.
func isNoSuchContainer(out string) bool {
	for _, marker := range []string{"not found", "no such", "does not exist"} {
		if strings.Contains(out, marker) {
			return true
		}
	}
	return false
}
