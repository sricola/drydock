package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"drydock/internal/brokerclient"
	"drydock/internal/webui"
)

func mintToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// uiURL builds the launch URL. The token rides in the fragment (#t=) so it is
// never sent in Referer headers or written to server logs.
func uiURL(port int, token string) string {
	base := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if token == "" {
		return base
	}
	return base + "#t=" + token
}

// runUI starts the web UI server. Deliberate loopback-only posture:
//   - Always binds 127.0.0.1 (never 0.0.0.0) so remote machines can't reach the
//     UI without an explicit SSH tunnel — the UI can submit tasks, approve diffs,
//     and kill running work.
//   - --open uses macOS `open` only (best-effort). xdg-open is intentionally
//     omitted for now; add it when Linux support is confirmed.
//   - Ignores BROKER_ADDR: the UI makes its own loopback socket connection to
//     brokerd via the Unix socket, not via BROKER_ADDR, so remote-broker configs
//     don't accidentally expose the UI to a foreign brokerd.
func runUI(args []string) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	port := fs.Int("port", 7878, "loopback port to bind")
	open := fs.Bool("open", false, "open the URL in the default browser")
	noToken := fs.Bool("no-token", false, "disable the access token (trusted single-user machines only)")
	_ = fs.Parse(args)

	token := ""
	if !*noToken {
		t, err := mintToken()
		if err != nil {
			die("mint token: %v", err)
		}
		token = t
	}

	srv := &webui.Server{
		AuditRoot:  auditDir(),
		Token:      token,
		BrokerDial: func() (net.Conn, error) { return net.Dial("unix", brokerclient.ResolveSocketPath()) },
	}

	addr := uiAddr(*port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		die("cannot bind %s: %v (is another `drydock ui` running, or the port taken?)", addr, err)
	}

	url := uiURL(*port, token)
	if *noToken {
		fmt.Fprintln(os.Stderr, "WARNING: --no-token — any local process or web page can submit tasks, approve pushes, and kill tasks via this server.")
	}
	fmt.Printf("UI ready: %s\n", url)
	if *open {
		_ = exec.Command("open", url).Start() // macOS; best-effort
	}
	// ReadHeaderTimeout bounds a slow-loris client holding the loopback
	// listener's connections open, matching brokerd's own server. No
	// ReadTimeout/WriteTimeout: /api/submit legitimately waits on brokerd.
	hs := newUIServer(srv.Handler())
	if err := hs.Serve(ln); err != nil {
		die("ui server: %v", err)
	}
}

// uiAddr is the loopback-only bind address: the UI is never exposed beyond
// this machine (web-ui.md), whatever port is chosen.
func uiAddr(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// newUIServer wraps the handler with the header timeout that bounds a
// slow-loris client; no body timeouts, since /api/submit legitimately waits.
func newUIServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
}
