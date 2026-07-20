package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"drydock/internal/brokerclient"
)

// errOut is the writer used by printClientErr. Tests replace it to capture
// output without redirecting the global os.Stderr. Production code is
// unaffected: errOut is initialised to os.Stderr and never reassigned at
// runtime.
var errOut io.Writer = os.Stderr

// taskState mirrors broker.TaskState. We don't import the broker package
// to keep the CLI lean; the shape is small and the JSON contract is stable.
type taskState struct {
	ID          string    `json:"id"`
	Repo        string    `json:"repo"`
	Instruction string    `json:"instruction"`
	Stage       string    `json:"stage"`
	StartedAt   time.Time `json:"started_at"`
	EgressExtra []domain  `json:"egress_extra,omitempty"`
}

type domain struct {
	Host  string `json:"host"`
	Ports []int  `json:"ports"`
}

func fetchTasks() ([]taskState, error) {
	c, base := brokerClient()
	resp, err := c.Get(base + "/admin/tasks")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /admin/tasks: brokerd returned %s", resp.Status)
	}
	var out []taskState
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse /admin/tasks: %w", err)
	}
	return out, nil
}

// socketPath returns the Unix socket path for the broker. Delegates to the
// shared brokerclient resolver so env/config logic lives in one place.
func socketPath() string { return brokerclient.ResolveSocketPath() }

// brokerClient returns an HTTP client and base URL for talking to the broker.
// Resolution order: BROKER_ADDR env → config.yaml broker.addr → Unix socket
// (via socketPath). Delegates all client construction to the shared helper.
func brokerClient() (*http.Client, string) {
	return brokerclient.New(nil, 5*time.Second)
}

// brokerdDown reports whether err comes from "brokerd isn't running" — the
// unix socket file is missing, or the dial was refused. We use it so the
// CLI can say something the user can act on ("start brokerd with `drydock
// start`") instead of dumping the raw Go HTTP transport error.
func brokerdDown(err error) bool {
	if err == nil {
		return false
	}
	if brokerclient.ResolveAddr() != "" {
		// TCP mode (BROKER_ADDR env or config.yaml broker.addr) — most "refused"
		// looks the same; don't second-guess with the socket-file hint.
		return false
	}
	if _, ferr := os.Stat(socketPath()); os.IsNotExist(ferr) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") || strings.Contains(s, "no such file or directory")
}

// brokerDownHint is the line printed when brokerdDown returns true. Keep
// it actionable: name the binary the user runs to fix it.
const brokerDownHint = "brokerd not running — start it in another shell with `drydock start`"

// printClientErr writes a single line to stderr. For "broker is down"
// errors it substitutes the friendly hint instead of dumping the raw Go
// HTTP transport error a first-time user can't act on.
func printClientErr(err error) {
	if brokerdDown(err) {
		fmt.Fprintln(errOut, "drydock:", brokerDownHint)
		return
	}
	fmt.Fprintf(errOut, "drydock: %v\n", err)
}

func listPending() {
	tasks, err := fetchTasks()
	if err != nil {
		printClientErr(err)
		os.Exit(1)
	}
	var pending []taskState
	for _, t := range tasks {
		if t.Stage == "awaiting_approval" || t.Stage == "awaiting_egress" {
			pending = append(pending, t)
		}
	}
	if len(pending) == 0 {
		fmt.Println("(no pending tasks)")
		return
	}
	fmt.Printf("%-14s  %5s  %-7s  %-24s  %-20s  %s\n", "ID", "AGE", "GATE", "REPO", "FLAGS", "DETAIL")
	dir := auditDir()
	for _, t := range pending {
		repo := shorten(t.Repo, 24)
		var gate, detail string
		switch t.Stage {
		case "awaiting_egress":
			gate = "egress"
			detail = formatExtras(t.EgressExtra)
		default: // awaiting_approval
			gate = "diff"
			detail = singleLine(t.Instruction)
		}
		// Flag kinds are broker-authored stable identifiers, but route them
		// through safeCell like every other brief-sourced string on this
		// column, and truncate by rune (not byte) so a control char or a
		// multi-byte rune sitting at the cap can't corrupt the terminal or
		// split mid-rune.
		flags := safeCell(briefFlagKinds(dir, t.ID))
		if r := []rune(flags); len(r) > 20 {
			flags = string(r[:19]) + "…"
		}
		fmt.Printf("%-14s  %5s  %-7s  %-24s  %-20s  %s\n", t.ID, relAge(t.StartedAt), gate, repo, flags, detail)
	}
	fmt.Println("\ntip: `drydock ui` reviews these diffs in a browser.")
}

func formatExtras(extras []domain) string {
	if len(extras) == 0 {
		return "(no hosts?)"
	}
	parts := make([]string, 0, len(extras))
	for _, d := range extras {
		ports := make([]string, 0, len(d.Ports))
		for _, p := range d.Ports {
			ports = append(ports, fmt.Sprintf("%d", p))
		}
		parts = append(parts, d.Host+":"+strings.Join(ports, ","))
	}
	return strings.Join(parts, " ")
}

// shorten trims a repo URL to its tail (owner/repo) for column width.
func shorten(s string, max int) string {
	if i := strings.LastIndex(s, ":"); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

func singleLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// pastTense renders an admin verb's completed-action confirmation. A blanket
// "%sd" misspells deny→"denyd", so map the known verbs explicitly.
func pastTense(verb string) string {
	switch verb {
	case "approve":
		return "approved"
	case "deny":
		return "denied"
	default:
		return verb + "d"
	}
}

func signal(verb, id string) {
	c, base := brokerClient()
	resp, err := c.Post(base+"/admin/"+verb+"/"+id, "", nil)
	if err != nil {
		printClientErr(err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		fmt.Printf("task %s %s\n", id, pastTense(verb))
	case http.StatusNotFound:
		fmt.Fprintf(os.Stderr, "drydock: no such pending task: %s\n", id)
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "drydock: brokerd returned %s\n", resp.Status)
		os.Exit(1)
	}
}
