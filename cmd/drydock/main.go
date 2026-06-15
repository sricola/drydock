// drydock is the operator CLI. Wraps brokerd's admin HTTP API so a human
// reviewing a pending diff can approve or deny without reaching for curl.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

const defaultSocket = "/tmp/drydock.sock"

func usage() {
	fmt.Fprint(os.Stderr, `drydock — operator CLI for the local brokerd

Usage:
  drydock pending                list task IDs awaiting approval
  drydock approve <task-id>      approve the pending push for task-id
  drydock deny    <task-id>      deny the pending push (diff is returned but not pushed)

Connection:
  Defaults to unix://`+defaultSocket+`. Override with BROKER_ADDR=host:port for TCP brokerd.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "pending":
		listPending()
	case "approve":
		if len(os.Args) != 3 {
			usage()
			os.Exit(2)
		}
		signal("approve", os.Args[2])
	case "deny":
		if len(os.Args) != 3 {
			usage()
			os.Exit(2)
		}
		signal("deny", os.Args[2])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func client() (*http.Client, string) {
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
	return c, "http://brokerd" // host part is unused for unix dial
}

func listPending() {
	c, base := client()
	resp, err := c.Get(base + "/admin/pending")
	if err != nil {
		fmt.Fprintf(os.Stderr, "drydock: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	var ids []string
	if err := json.NewDecoder(resp.Body).Decode(&ids); err != nil {
		fmt.Fprintf(os.Stderr, "drydock: parse response: %v\n", err)
		os.Exit(1)
	}
	if len(ids) == 0 {
		fmt.Println("(no pending tasks)")
		return
	}
	for _, id := range ids {
		fmt.Println(id)
	}
}

func signal(verb, id string) {
	c, base := client()
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
