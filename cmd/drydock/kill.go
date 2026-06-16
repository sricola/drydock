package main

import (
	"fmt"
	"net/http"
	"os/exec"
)

// runKill makes a best effort to stop a task at any stage: if the VM is
// running, `container delete --force task-<id>` tears it down; if the
// task is waiting for approval, we deny it. No brokerd-side endpoint
// needed today — both moves are visible to the operator's container CLI
// and the existing /admin/deny.
func runKill(id string) {
	// 1) Tear down the VM if it's running.
	out, err := exec.Command("container", "delete", "--force", "task-"+id).CombinedOutput()
	switch {
	case err == nil:
		fmt.Printf("task %s VM removed\n", id)
	case isNoSuchContainer(string(out)):
		// fine — task may have already exited
	default:
		fmt.Fprintf(stderr(), "drydock kill: container delete: %v\n%s", err, out)
	}

	// 2) If the task is sitting at the approval gate, deny it.
	c, base := brokerClient()
	resp, err := c.Post(base+"/admin/deny/"+id, "", nil)
	if err != nil {
		// brokerd down or unreachable — VM kill above is enough.
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		fmt.Printf("task %s denied at the approval gate\n", id)
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
