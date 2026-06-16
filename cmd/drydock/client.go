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
	c, base := brokerClient()
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
