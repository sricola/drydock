package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runStatus prints a compact one-screen status. If brokerd is up, healthz
// answers with a stage breakdown; if not, we say so. Then we summarise
// recent runs from AUDIT_ROOT.
func runStatus() {
	if h, err := health(); err == nil {
		fmt.Printf("brokerd     up\n")
		fmt.Printf("in flight   %d running · %d awaiting egress · %d awaiting diff · %d pushing\n",
			h.Running, h.AwaitingEgress, h.PendingApproval, h.Pushing)
	} else if brokerdDown(err) {
		fmt.Printf("brokerd     down — start it with `drydock start`\n")
	} else {
		fmt.Printf("brokerd     down (%v)\n", err)
	}

	dir := auditDir()
	entries, _ := os.ReadDir(dir)
	var total int
	var last24 int
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		total++
		info, err := e.Info()
		if err == nil && info.ModTime().After(cutoff) {
			last24++
		}
	}
	fmt.Printf("tasks       %d total · %d in last 24h\n", total, last24)
	fmt.Printf("audit dir   %s\n", filepath.Clean(dir))
	fmt.Println("tip: `drydock ui` opens a local web dashboard (review diffs, submit, history).")
}

type healthBody struct {
	OK              bool `json:"ok"`
	AwaitingEgress  int  `json:"awaiting_egress"`
	Running         int  `json:"running"`
	PendingApproval int  `json:"pending_approval"`
	Pushing         int  `json:"pushing"`
}

func health() (healthBody, error) {
	c, base := brokerClient()
	resp, err := c.Get(base + "/healthz")
	if err != nil {
		return healthBody{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return healthBody{}, fmt.Errorf("GET /healthz: brokerd returned %s", resp.Status)
	}
	var body healthBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return healthBody{}, fmt.Errorf("parse health: %w", err)
	}
	return body, nil
}
