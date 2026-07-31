package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// `drydock queue` — the CLI for the durable task queue (POST /queue,
// GET /queue, POST /queue/cancel/{id}). Unlike `submit`, `queue add` returns
// the moment brokerd has persisted the item: the task runs unattended when a
// concurrency slot frees, survives brokerd restarts, and (unless
// --auto-approve) still stops at the diff-approval gate like any other task.

func queueUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: drydock queue <add|list|cancel>

  drydock queue add <submit flags>   enqueue a task; returns immediately with its id
  drydock queue list                 show queue items: id, state, age, attempts,
                                     ci, retry chain, repo
  drydock queue cancel <id>          cancel a queued item (kills it if already
                                     running; cancels the CI watch if it is
                                     parked in awaiting_ci)

'queue add' takes the same flags as 'drydock submit' (drydock queue add -h).
Queued tasks survive brokerd restarts and run unattended when a concurrency
slot frees; each diff still stops at the approval gate unless --auto-approve.
`)
}

func runQueue(args []string) {
	if len(args) == 0 {
		queueUsage(os.Stderr)
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		runQueueAdd(rest)
	case "list":
		runQueueList()
	case "cancel":
		if len(rest) != 1 {
			die("usage: drydock queue cancel <id>")
		}
		os.Exit(postQueueCancel(rest[0]))
	case "-h", "--help", "help":
		queueUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "drydock queue: unknown subcommand %q\n\n", sub)
		queueUsage(os.Stderr)
		os.Exit(2)
	}
}

// queueAddUsageIntro/-Outro wrap the SAME flag defaults submit prints — the
// flag surface itself comes from the shared parseTaskRequest builder.
const queueAddUsageIntro = `Usage: drydock queue add [flags]

Enqueue a task on brokerd's durable queue and return immediately. The task
runs unattended when a concurrency slot frees (and its vendor is under the
aggregate spend cap), survives brokerd restarts, and still stops at the
diff-approval gate unless --auto-approve.

Flags:
`

const queueAddUsageOutro = `
Examples:
  drydock queue add --repo git@github.com:o/r --instruction "fix the test"
  drydock queue add --issue https://github.com/o/r/issues/42 --auto-approve
  drydock queue add --repo git@github.com:o/r --instruction-file ./task.md

Watch it with 'drydock queue list'; cancel with 'drydock queue cancel <id>'.

Connection: respects BROKER_SOCKET / BROKER_ADDR (same as the other
admin subcommands). Default is the per-uid socket discovered via
sockpath.Default().`

// parseQueueAddRequest parses `drydock queue add`'s argv via the SAME builder
// `drydock submit` uses (parseTaskRequest, submit.go) — identical flags,
// identical issue ingestion, identical instruction resolution.
func parseQueueAddRequest(args []string) (taskRequest, bool, bool) {
	return parseTaskRequest("drydock queue add", queueAddUsageIntro, queueAddUsageOutro, args)
}

func runQueueAdd(args []string) {
	req, jsonOut, _ := parseQueueAddRequest(args) // --quiet is meaningless here: nothing streams
	id, err := postQueueAdd(req)
	if err != nil {
		die("%v", err)
	}
	if jsonOut {
		out, _ := json.Marshal(map[string]string{"event": "queued", "task_id": id})
		fmt.Println(string(out))
		return
	}
	fmt.Printf("queued %s\n", id)
	fmt.Println("  runs unattended when a slot frees — `drydock queue list` to watch, `drydock queue cancel " + id + "` to cancel")
}

// postQueueAdd POSTs the task to /queue and returns the minted task id. The
// short-timeout admin client is deliberate: unlike /tasks, /queue responds
// immediately (no stream to hold open).
func postQueueAdd(req taskRequest) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	c, base := brokerClient()
	resp, err := c.Post(base+"/queue", "application/json", bytes.NewReader(body))
	if err != nil {
		if brokerdDown(err) {
			return "", errors.New(brokerDownHint)
		}
		return "", fmt.Errorf("brokerd: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("parse /queue response: %w", err)
	}
	if out.TaskID == "" {
		return "", errors.New("brokerd returned no task_id")
	}
	return out.TaskID, nil
}

// queueListItem mirrors broker's projected queueItemView. Like taskState in
// client.go we don't import the broker package; the JSON contract is stable.
type queueListItem struct {
	ID           string `json:"id"`
	Repo         string `json:"repo"`
	State        string `json:"state"`
	EnqueuedAtMs int64  `json:"enqueued_at_ms"`
	Attempts     int    `json:"attempts"`
	PRNumber     int    `json:"pr_number"`
	CIState      string `json:"ci_state"`
	RetryOf      string `json:"retry_of"`
	RetryTaskID  string `json:"retry_task_id"`
	Attempt      int    `json:"attempt"`
}

func fetchQueue() ([]queueListItem, error) {
	c, base := brokerClient()
	resp, err := c.Get(base + "/queue")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /queue: brokerd returned %s", resp.Status)
	}
	var out []queueListItem
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse /queue: %w", err)
	}
	return out, nil
}

// queueCICell renders the CI column: the broker's last OBSERVED conclusion for
// the item's pull request, with the PR number when there is one.
//
// The empty CI state renders as "-", NEVER as anything that could be mistaken
// for a pass. It means one of: the watch is off, this item never pushed, or no
// PR identity was captured — in every case the broker has observed nothing.
// The states themselves are broker-authored vocabulary (pending, passed,
// failed, no_checks, timed_out, unknown); no CI log text exists in the payload
// at all (D3). It still goes through safeCell like every other broker-sourced
// string: this column is a terminal write, and the sanitizer is what keeps a
// tampered queue file from putting escapes on an operator's screen.
func queueCICell(it queueListItem) string {
	switch {
	case it.CIState == "" && it.PRNumber == 0:
		return "-"
	case it.CIState == "":
		return "#" + fmt.Sprint(it.PRNumber)
	case it.PRNumber == 0:
		return safeCell(it.CIState)
	default:
		return safeCell(it.CIState) + " #" + fmt.Sprint(it.PRNumber)
	}
}

// queueRetryCell renders the RETRY column: this item's place in a bounded
// CI-retry chain, which is otherwise only visible by reading the audit dir by
// hand.
//
// A lone "-" means "not in a chain", which is every item on a stock install
// (ci.max_attempts is 0 by default). Otherwise the cell leads with this item's
// attempt number and then points: "<-" at the parent it retries, "->" at the
// retry its own observed CI failure enqueued, or both for a middle link. So
// "#2 <-1a2b3c4d5e6f… ->9f8e7d6c5b4a…" reads as "attempt 2, retrying that,
// retried by this".
//
// Ids only, and abbreviated for width — the full ids are on `GET /queue` as
// retry_of/retry_task_id. They are broker-minted 32-hex values, but they still
// cross the terminal boundary through safeCell like every other cell: a
// tampered queue file must not be able to put escapes on an operator's screen.
func queueRetryCell(it queueListItem) string {
	if it.RetryOf == "" && it.RetryTaskID == "" {
		return "-"
	}
	parts := []string{fmt.Sprintf("#%d", it.Attempt)}
	if it.RetryOf != "" {
		parts = append(parts, "<-"+shortID(it.RetryOf))
	}
	if it.RetryTaskID != "" {
		parts = append(parts, "->"+shortID(it.RetryTaskID))
	}
	return strings.Join(parts, " ")
}

// shortID abbreviates a task id to a column-sized prefix. Deliberately not
// `shorten`, which is repo-URL-shaped (it splits on the last ':'): an id is an
// opaque token and must be truncated from the left, not parsed. Sanitized
// first, then cut, so the cut can never land inside a multi-byte rune the
// sanitizer would have dropped anyway.
func shortID(id string) string {
	s := safeCell(id)
	r := []rune(s)
	if len(r) > 12 {
		return string(r[:12]) + "…"
	}
	return s
}

// renderQueueTable renders the queue as ID/STATE/AGE/ATTEMPTS/CI/RETRY/REPO,
// oldest first (brokerd already sorts by enqueue time). AGE is derived from
// enqueued_at_ms. Every broker-sourced string crosses the terminal boundary
// through safeCell, same as the other tables.
func renderQueueTable(w io.Writer, items []queueListItem) {
	if len(items) == 0 {
		fmt.Fprintln(w, "(queue is empty)")
		return
	}
	fmt.Fprintf(w, "%-32s  %-15s  %5s  %8s  %-16s  %-32s  %s\n",
		"ID", "STATE", "AGE", "ATTEMPTS", "CI", "RETRY", "REPO")
	for _, it := range items {
		fmt.Fprintf(w, "%-32s  %-15s  %5s  %8d  %-16s  %-32s  %s\n",
			safeCell(it.ID), safeCell(it.State),
			relAge(time.UnixMilli(it.EnqueuedAtMs)), it.Attempts,
			queueCICell(it), queueRetryCell(it),
			safeCell(shorten(it.Repo, 40)))
	}
}

func runQueueList() {
	items, err := fetchQueue()
	if err != nil {
		printClientErr(err)
		os.Exit(1)
	}
	renderQueueTable(os.Stdout, items)
}

// postQueueCancel cancels a queue item and returns the process exit code
// (matching the doSignal/runKill exit contract). Brokerd dequeues a still-
// queued item or delegates to the live-task kill; 404 means neither exists.
func postQueueCancel(id string) int {
	c, base := brokerClient()
	resp, err := c.Post(base+"/queue/cancel/"+id, "", nil)
	if err != nil {
		printClientErr(err)
		return 1
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		fmt.Printf("task %s cancelled\n", id)
		return 0
	case http.StatusNotFound:
		fmt.Fprintf(errOut, "drydock queue cancel: no such queued, running, or ci-watched task: %s\n", id)
		return 1
	default:
		fmt.Fprintf(errOut, "drydock queue cancel: brokerd returned %s\n", resp.Status)
		return 1
	}
}
