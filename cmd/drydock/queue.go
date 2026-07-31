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
  drydock queue list                 show queue items: id, state, age, attempts, repo
  drydock queue cancel <id>          cancel a queued item (kills it if already running)

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

// renderQueueTable renders the queue as ID/STATE/AGE/ATTEMPTS/REPO, oldest
// first (brokerd already sorts by enqueue time). AGE is derived from
// enqueued_at_ms. Every broker-sourced string crosses the terminal boundary
// through safeCell, same as the other tables.
func renderQueueTable(w io.Writer, items []queueListItem) {
	if len(items) == 0 {
		fmt.Fprintln(w, "(queue is empty)")
		return
	}
	fmt.Fprintf(w, "%-32s  %-15s  %5s  %8s  %s\n", "ID", "STATE", "AGE", "ATTEMPTS", "REPO")
	for _, it := range items {
		fmt.Fprintf(w, "%-32s  %-15s  %5s  %8d  %s\n",
			safeCell(it.ID), safeCell(it.State),
			relAge(time.UnixMilli(it.EnqueuedAtMs)), it.Attempts,
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
		fmt.Fprintf(errOut, "drydock queue cancel: no such queued or running task: %s\n", id)
		return 1
	default:
		fmt.Fprintf(errOut, "drydock queue cancel: brokerd returned %s\n", resp.Status)
		return 1
	}
}
