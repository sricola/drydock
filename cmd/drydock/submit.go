package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	ossignal "os/signal"
	"strconv"
	"strings"
	"syscall"
)

// taskRequest mirrors the broker.Task JSON shape. We don't import broker
// to keep the CLI lean; the contract is stable.
type taskRequest struct {
	RepoRef     string      `json:"repo_ref"`
	Instruction string      `json:"instruction"`
	EgressExtra []reqDomain `json:"egress_extra,omitempty"`
	Sensitive   bool        `json:"sensitive,omitempty"`
	AutoApprove bool        `json:"auto_approve,omitempty"`
	Platform    string      `json:"platform,omitempty"`
	Model       string      `json:"model,omitempty"`
	Agent       string      `json:"agent,omitempty"`
}

type reqDomain struct {
	Host  string `json:"host"`
	Ports []int  `json:"ports"`
}

// repeatedFlag is a string flag that can be passed more than once.
type repeatedFlag []string

func (r *repeatedFlag) String() string     { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(v string) error { *r = append(*r, v); return nil }

func runSubmit(args []string) {
	fs := flag.NewFlagSet("drydock submit", flag.ExitOnError)
	var (
		repo        = fs.String("repo", "", "repo ref (https/git/ssh URL; required)")
		instruction = fs.String("instruction", "", "what the agent should do (use - or omit to read stdin)")
		instrFile   = fs.String("instruction-file", "", "path to a file holding the instruction")
		autoApprove = fs.Bool("auto-approve", false, "skip the diff-push gate (use only for trusted batch runs)")
		platform    = fs.String("platform", "", "github | gitlab | gitea | none (default: autodetect)")
		model       = fs.String("model", "", "claude --model passthrough (e.g. claude-opus-4-7); empty = use broker default")
		agent       = fs.String("agent", "", "sandbox agent: claude | codex (default: broker's default_agent)")
		sensitive   = fs.Bool("sensitive", false, "mark the task sensitive in the audit trail")
		jsonOut     = fs.Bool("json", false, "print the raw response JSON instead of a pretty summary")
		quiet       = fs.Bool("quiet", false, "suppress live progress; print only the final outcome")
		egress      repeatedFlag
	)
	fs.Var(&egress, "egress-extra", "extra egress host:port[,port] (repeatable)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: drydock submit [flags]

POST a task to the running brokerd. Blocks until the agent run completes
and (unless --auto-approve) you approve or deny the diff in another shell.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintln(os.Stderr, `
Examples:
  drydock submit --repo git@github.com:o/r --instruction "fix the test"
  drydock submit --repo git@github.com:o/r --instruction-file ./task.md
  echo "do thing" | drydock submit --repo git@github.com:o/r -
  drydock submit --repo git@gitlab.mycorp.com:g/p --platform gitlab \
                 --egress-extra internal.example.com:443 --auto-approve

Connection: respects BROKER_SOCKET / BROKER_ADDR (same as the other
admin subcommands). Default is the per-uid socket discovered via
sockpath.Default().`)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *repo == "" {
		fmt.Fprintln(os.Stderr, "drydock submit: --repo is required")
		fs.Usage()
		os.Exit(2)
	}

	instr, err := readInstruction(*instruction, *instrFile, fs.Args())
	if err != nil {
		die("%v", err)
	}
	if instr == "" {
		die("instruction is empty")
	}

	extras, err := parseEgressExtras([]string(egress))
	if err != nil {
		die("--egress-extra: %v", err)
	}

	req := taskRequest{
		RepoRef:     *repo,
		Instruction: instr,
		EgressExtra: extras,
		Sensitive:   *sensitive,
		AutoApprove: *autoApprove,
		Platform:    *platform,
		Model:       *model,
		Agent:       *agent,
	}
	if err := postSubmit(req, *jsonOut, *quiet); err != nil {
		die("%v", err)
	}
}

// readInstruction resolves the instruction text from flags / file / stdin /
// positional - argument, in that priority. Empty string is allowed only when
// the caller explicitly passed "-".
func readInstruction(inline, file string, positional []string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", file, err)
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	if inline != "" && inline != "-" {
		return inline, nil
	}
	// "-" or empty + a "-" positional → stdin.
	if inline == "-" || (len(positional) > 0 && positional[0] == "-") {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	// No instruction sources at all → read from stdin if it's piped.
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return strings.TrimRight(string(b), "\n"), nil
	}
	return "", errors.New("no instruction (use --instruction, --instruction-file, or pipe to stdin)")
}

// parseEgressExtras parses N strings of form "host:port[,port,port]" into
// the structured shape brokerd expects.
func parseEgressExtras(raw []string) ([]reqDomain, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]reqDomain, 0, len(raw))
	for _, entry := range raw {
		i := strings.Index(entry, ":")
		if i <= 0 || i == len(entry)-1 {
			return nil, fmt.Errorf("expected host:port[,port], got %q", entry)
		}
		host := entry[:i]
		portStrs := strings.Split(entry[i+1:], ",")
		ports := make([]int, 0, len(portStrs))
		for _, p := range portStrs {
			p = strings.TrimSpace(p)
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil, fmt.Errorf("bad port in %q: %s", entry, p)
			}
			ports = append(ports, n)
		}
		out = append(out, reqDomain{Host: host, Ports: ports})
	}
	return out, nil
}

// postSubmit builds an HTTP client without a timeout (tasks can run for
// 30+ minutes plus operator approval), POSTs /tasks, and prints the
// response. SIGINT propagates as a context cancel — brokerd treats that
// as a task kill, so ^C is a clean abort.
func postSubmit(req taskRequest, jsonOut, quiet bool) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}

	// Long-lived client: no timeout. Tasks legitimately take tens of
	// minutes; the existing 5s-timeout brokerClient is for admin pokes.
	var client *http.Client
	var base string
	if tcp := os.Getenv("BROKER_ADDR"); tcp != "" {
		client = &http.Client{}
		base = "http://" + tcp
	} else {
		sock := socketPath()
		client = &http.Client{
			Transport: &http.Transport{
				DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", sock)
				},
			},
		}
		base = "http://brokerd"
	}

	// ^C in the submit shell cancels the in-flight POST instead of leaving
	// the local CLI dead and brokerd quietly running the task. brokerd's
	// HandleTask reads the request context and treats cancellation as a
	// task kill (the comment a few lines above is now actually true).
	ctx, stop := ossignal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		base+"/tasks", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		if brokerdDown(err) {
			return fmt.Errorf("%s", brokerDownHint)
		}
		return fmt.Errorf("brokerd: %w", err)
	}
	defer resp.Body.Close()

	if jsonOut {
		// Raw passthrough — works for the NDJSON stream or a legacy object,
		// and streams live so scripts see events as they arrive.
		_, _ = io.Copy(os.Stdout, resp.Body)
		// StatusCode comes from the response headers (received before the body),
		// so checking it after the copy still reflects the real status.
		if resp.StatusCode >= 400 {
			os.Exit(1)
		}
		return nil
	}

	// Pre-accept failures keep an HTTP error status + plain body (the stream
	// never started). Surface them directly.
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "drydock submit: HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		os.Exit(1)
	}

	mode := modePiped
	if tty { // package-level, defined in init.go
		mode = modeTTY
	}
	if quiet {
		mode = modeQuiet
	}
	if exit := consume(resp.Body, os.Stdout, mode); exit != 0 {
		os.Exit(exit)
	}
	return nil
}

func printPretty(w io.Writer, out map[string]any) {
	id, _ := out["task_id"].(string)
	switch {
	case out["cancelled"] == true:
		fmt.Fprintf(w, "task %s: cancelled\n", id)
		if diff, _ := out["diff"].(string); diff != "" {
			fmt.Fprintf(w, "  diff captured (%d bytes); inspect %s\n", len(diff), diffPath(id))
		}
	case out["pushed"] == true:
		branch, _ := out["branch"].(string)
		platform, _ := out["platform"].(string)
		fmt.Fprintf(w, "task %s: pushed %s (%s)\n", id, branch, platform)
	default:
		fmt.Fprintf(w, "task %s: not pushed\n", id)
		if diff, _ := out["diff"].(string); diff != "" {
			fmt.Fprintf(w, "  diff captured (%d bytes); inspect %s\n", len(diff), diffPath(id))
		} else {
			fmt.Fprintln(w, "  agent returned no diff")
		}
	}
}
