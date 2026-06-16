package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runStatus prints a compact one-screen status. If brokerd is up, healthz
// answers; if not, we say so. Then we summarise recent runs from AUDIT_ROOT.
func runStatus() {
	if up, pending, err := health(); err == nil {
		_ = up
		fmt.Printf("brokerd     up\n")
		fmt.Printf("pending     %d\n", pending)
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
	fmt.Printf("tasks       %d total / %d in last 24h\n", total, last24)
	fmt.Printf("audit dir   %s\n", filepath.Clean(dir))
}

func health() (bool, int, error) {
	c, base := brokerClient()
	resp, err := c.Get(base + "/healthz")
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	var body struct {
		OK      bool `json:"ok"`
		Pending int  `json:"pending"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, 0, fmt.Errorf("parse health: %w", err)
	}
	return body.OK, body.Pending, nil
}
