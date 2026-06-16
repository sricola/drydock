package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// taskState mirrors broker.TaskState. We don't import the broker package
// to keep the CLI lean; the shape is small and the JSON contract is stable.
type taskState struct {
	ID          string    `json:"id"`
	Repo        string    `json:"repo"`
	Instruction string    `json:"instruction"`
	Stage       string    `json:"stage"`
	StartedAt   time.Time `json:"started_at"`
}

func fetchTasks() ([]taskState, error) {
	c, base := brokerClient()
	resp, err := c.Get(base + "/admin/tasks")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []taskState
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse /admin/tasks: %w", err)
	}
	return out, nil
}

const defaultSocket = "/tmp/drydock.sock"

func brokerClient() (*http.Client, string) {
	if tcp := os.Getenv("BROKER_ADDR"); tcp != "" {
		return &http.Client{Timeout: 5 * time.Second}, "http://" + tcp
	}
	c := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", defaultSocket)
			},
		},
	}
	return c, "http://brokerd"
}

func listPending() {
	tasks, err := fetchTasks()
	if err != nil {
		fmt.Fprintf(os.Stderr, "drydock: %v\n", err)
		os.Exit(1)
	}
	var pending []taskState
	for _, t := range tasks {
		if t.Stage == "awaiting_approval" {
			pending = append(pending, t)
		}
	}
	if len(pending) == 0 {
		fmt.Println("(no pending tasks)")
		return
	}
	fmt.Printf("%-14s  %5s  %-32s  %s\n", "ID", "AGE", "REPO", "INSTRUCTION")
	for _, t := range pending {
		repo := shorten(t.Repo, 32)
		instr := singleLine(t.Instruction)
		fmt.Printf("%-14s  %5s  %-32s  %s\n", t.ID, relAge(t.StartedAt), repo, instr)
	}
}

// shorten trims a repo URL to its tail (owner/repo) for column width.
func shorten(s string, max int) string {
	if i := strings.LastIndex(s, ":"); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	} else if i := strings.LastIndex(s, "/owner/"); i >= 0 {
		// no-op; keep the suffix
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

func signal(verb, id string) {
	c, base := brokerClient()
	resp, err := c.Post(base+"/admin/"+verb+"/"+id, "", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "drydock: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		fmt.Printf("task %s %sd\n", id, verb)
	case http.StatusNotFound:
		fmt.Fprintf(os.Stderr, "drydock: no such pending task: %s\n", id)
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "drydock: brokerd returned %s\n", resp.Status)
		os.Exit(1)
	}
}
