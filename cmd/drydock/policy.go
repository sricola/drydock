package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"drydock/internal/config"
)

// runPolicy handles `drydock policy explain`: the resolved effective policy,
// each value's source layer (env > config.yaml > default), and — when brokerd
// is reachable — whether the running daemon resolved the same policy. Source
// attribution is value-based: a config.yaml line that sets a field to its
// default value reports as "default" (the layers agree, so the lowest wins).
//
// Args are scanned by hand (no flag.FlagSet) so --json is accepted in either
// position relative to the subcommand, matching runInspect.
func runPolicy(args []string) {
	var jsonOut bool
	var rest []string
	for _, a := range args {
		switch a {
		case "--json", "-json":
			jsonOut = true
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 || rest[0] != "explain" {
		die("usage: drydock policy explain [--json]")
	}

	fields, _, err := config.Explain(config.DefaultPath())
	if err != nil {
		die("policy explain: %v", err)
	}
	localHash := config.EffectiveHash(fields)

	live, liveErr := fetchLivePolicy()
	if liveErr != nil && !brokerdDown(liveErr) {
		// A reachable-but-failing daemon (5xx, bad body) is worth a stderr
		// line in both modes; the local view below is still rendered.
		printClientErr(liveErr)
	}

	if jsonOut {
		printPolicyJSON(fields, localHash, live, liveErr)
		return
	}

	renderPolicyTable(fields)
	fmt.Println()
	switch {
	case liveErr == nil:
		if live.Hash == localHash {
			fmt.Println("daemon: in sync — the running brokerd resolved this same policy")
		} else {
			fmt.Println("daemon: DIVERGENT — the running brokerd resolved a different policy (restart brokerd to pick up changes)")
			for _, d := range diffPolicyFields(fields, live.Fields) {
				fmt.Printf("  %s: local=%s live=%s\n", safeCell(d.name), safeCell(d.local), safeCell(d.live))
			}
		}
	case brokerdDown(liveErr):
		banner := "LIVE POLICY UNVERIFIED — brokerd not reachable; showing local config only"
		if tty {
			fmt.Printf("\x1b[1;33m%s\x1b[0m\n", banner)
		} else {
			fmt.Println(banner)
		}
	default:
		// GET failed for a non-down reason; already reported to stderr above.
		fmt.Println("daemon: live policy unavailable (see error above); showing local config only")
	}
}

// livePolicy mirrors GET /admin/policy's response body. Field is imported
// from internal/config rather than redeclared: the daemon marshals the same
// struct, so the JSON contract is shared by construction.
type livePolicy struct {
	Fields []config.Field `json:"fields"`
	Hash   string         `json:"hash"`
}

// fetchLivePolicy asks the running brokerd for the policy it resolved at
// boot. A short per-request timeout (tighter than brokerClient's default)
// keeps `policy explain` snappy when the daemon is wedged: the local table
// is the primary output and must not hang behind a sick daemon.
func fetchLivePolicy() (livePolicy, error) {
	c, base := brokerClient()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/admin/policy", nil)
	if err != nil {
		return livePolicy{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return livePolicy{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return livePolicy{}, fmt.Errorf("GET /admin/policy: brokerd returned %s", resp.Status)
	}
	var out livePolicy
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return livePolicy{}, fmt.Errorf("parse /admin/policy: %w", err)
	}
	return out, nil
}

// renderPolicyTable prints the aligned SETTING / VALUE / SOURCE table. Every
// name and value routes through safeCell like inspect's cells: values can
// embed operator-authored yaml strings, and stripping controls there is
// cheaper than arguing about which fields could carry them.
func renderPolicyTable(fields []config.Field) {
	type row struct{ name, value, source string }
	rows := make([]row, 0, len(fields))
	nameW, valueW := len("SETTING"), len("VALUE")
	for _, f := range fields {
		r := row{
			name:   safeCell(f.Name),
			value:  orDash(safeCell(f.Value)),
			source: policySourceLabel(f),
		}
		if len(r.name) > nameW {
			nameW = len(r.name)
		}
		if len(r.value) > valueW {
			valueW = len(r.value)
		}
		rows = append(rows, r)
	}
	fmt.Printf("%-*s  %-*s  %s\n", nameW, "SETTING", valueW, "VALUE", "SOURCE")
	for _, r := range rows {
		fmt.Printf("%-*s  %-*s  %s\n", nameW, r.name, valueW, r.value, r.source)
	}
}

// policySourceLabel renders a field's source column: "default", "config.yaml",
// or "env:VARNAME" so an env-sourced row names the variable to unset.
func policySourceLabel(f config.Field) string {
	if f.Source == config.SourceEnv {
		return safeCell("env:" + f.EnvVar)
	}
	return safeCell(string(f.Source))
}

// policyDiff is one field whose effective value differs between the local
// resolution and the running daemon's boot-time resolution.
type policyDiff struct{ name, local, live string }

// diffPolicyFields joins local and live fields by Name and returns the ones
// whose values differ. Fields present on only one side (version skew between
// CLI and daemon) surface as "(absent)" rather than being silently dropped —
// a missing field is itself divergence evidence.
func diffPolicyFields(local, live []config.Field) []policyDiff {
	liveVal := make(map[string]string, len(live))
	for _, f := range live {
		liveVal[f.Name] = f.Value
	}
	seen := make(map[string]bool, len(local))
	var out []policyDiff
	for _, f := range local {
		seen[f.Name] = true
		lv, ok := liveVal[f.Name]
		switch {
		case !ok:
			out = append(out, policyDiff{f.Name, f.Value, "(absent)"})
		case lv != f.Value:
			out = append(out, policyDiff{f.Name, f.Value, lv})
		}
	}
	for _, f := range live {
		if !seen[f.Name] {
			out = append(out, policyDiff{f.Name, "(absent)", f.Value})
		}
	}
	return out
}

// printPolicyJSON emits the machine-readable shape:
// {"local":{fields,hash}, "live":{fields,hash}|null, "in_sync":bool|null}.
// live/in_sync are null when the daemon could not be asked — absence of a
// verdict, distinct from either sync state.
func printPolicyJSON(fields []config.Field, localHash string, live livePolicy, liveErr error) {
	type side struct {
		Fields []config.Field `json:"fields"`
		Hash   string         `json:"hash"`
	}
	body := struct {
		Local  side  `json:"local"`
		Live   *side `json:"live"`
		InSync *bool `json:"in_sync"`
	}{Local: side{Fields: fields, Hash: localHash}}
	if liveErr == nil {
		body.Live = &side{Fields: live.Fields, Hash: live.Hash}
		inSync := live.Hash == localHash
		body.InSync = &inSync
	}
	b, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		die("marshal policy: %v", err)
	}
	fmt.Println(string(b))
}
