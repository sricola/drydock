package main

import (
	"fmt"
	"net/http"
	"os/exec"
)

// runKill cancels a task. Preferred path is POST /admin/kill, which fires
// the brokerd-side context cancel — the container exits cleanly, the
// approval gate (if reached) unblocks with cancelled=true, and the
// `POST /tasks` request returns a structured cancellation response.
//
// Falls back to `container delete --force task-<id>` only when brokerd is
// unreachable or doesn't know the task — for example, the operator killed
// brokerd itself and the orphan container is still up.
func runKill(id string) {
	c, base := brokerClient()
	resp, err := c.Post(base+"/admin/kill/"+id, "", nil)
	if err == nil {
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusNoContent:
			fmt.Printf("task %s cancelled\n", id)
			return
		case http.StatusNotFound:
			// brokerd is up but doesn't track this task; try the
			// container CLI in case it's an orphan.
		default:
			fmt.Fprintf(stderr(), "drydock kill: brokerd returned %s\n", resp.Status)
			return
		}
	}
	// Brokerd unreachable or 404 — best-effort VM cleanup.
	out, ferr := exec.Command("container", "delete", "--force", "task-"+id).CombinedOutput()
	switch {
	case ferr == nil:
		fmt.Printf("task %s VM removed (brokerd didn't know about it)\n", id)
	case isNoSuchContainer(string(out)):
		fmt.Fprintf(stderr(), "drydock kill: no such task %s\n", id)
	default:
		fmt.Fprintf(stderr(), "drydock kill: container delete: %v\n%s", ferr, out)
	}
}

// isNoSuchContainer recognises Apple container's "container not found"
// shape so we don't surface a noisy error when the task already exited.
func isNoSuchContainer(out string) bool {
	for _, marker := range []string{"not found", "no such", "does not exist"} {
		if contains(out, marker) {
			return true
		}
	}
	return false
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
