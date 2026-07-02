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

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
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
	if err := http.Serve(ln, srv.Handler()); err != nil {
		die("ui server: %v", err)
	}
}
