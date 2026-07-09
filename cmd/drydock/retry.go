package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// readInvocation scans an audit trace for the {"type":"drydock_task",...} line
// the broker persists and decodes it into a taskRequest. ok=false when no such
// line is present (a task that predates retry, or a truncated trace).
func readInvocation(r io.Reader) (taskRequest, bool) {
	var req taskRequest
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // instructions can be large
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"drydock_task"`) {
			continue
		}
		if json.Unmarshal(line, &req) == nil && req.RepoRef != "" {
			return req, true
		}
	}
	return taskRequest{}, false
}

// runRetry re-submits a prior task from the invocation the broker persisted in
// its trace (a {"type":"drydock_task",...} line: repo, instruction, agent,
// model, platform, egress, draft, sensitive). It saves the operator
// reconstructing the whole `drydock submit` invocation by hand. auto_approve is
// never carried over — a retry re-enters the approval gate unless the operator
// opts back in.
func runRetry(id string) {
	f, err := os.Open(auditPath(id))
	if err != nil {
		die("%v", err) // err already includes the path
	}
	defer f.Close()

	req, found := readInvocation(f)
	if !found {
		die("task %s has no recorded invocation to retry (predates retry, or the trace was truncated)", id)
	}

	req.AutoApprove = false // re-gate; the operator re-opts-in with --auto-approve if wanted
	fmt.Printf("retrying %s → %s\n", id, req.RepoRef)
	if err := postSubmit(req, false, false); err != nil {
		die("%v", err)
	}
}
