// Package brokerclient provides a constructor for the HTTP client used to
// talk to brokerd over its unix socket or a TCP fallback address.
//
// Resolution order (when dialFn is nil):
//
//	BROKER_ADDR env → config.yaml broker.addr → unix socket
//	  (BROKER_SOCKET env → config.yaml broker.socket → sockpath.Default())
//
// Callers that have already resolved a dial function (e.g. internal/webui,
// which receives one via injection) pass it as dialFn and skip env/config
// resolution entirely.
package brokerclient

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	"drydock/internal/config"
	"drydock/internal/sockpath"
)

// New returns an (*http.Client, baseURL) for talking to brokerd.
//
// If dialFn is non-nil it is used directly as the unix-socket dialer and
// baseURL is "http://brokerd". No env/config resolution is performed.
//
// If dialFn is nil, resolution proceeds: BROKER_ADDR env → config.yaml
// broker.addr → unix socket (BROKER_SOCKET env → config.yaml broker.socket
// → sockpath.Default()).
//
// timeout = 0 means no client-level timeout; pass 0 for long-running
// streaming calls (e.g. task submission) and a positive value for short
// admin polls.
func New(dialFn func() (net.Conn, error), timeout time.Duration) (*http.Client, string) {
	if dialFn != nil {
		return newUnix(dialFn, timeout), "http://brokerd"
	}
	if addr := ResolveAddr(); addr != "" {
		c := &http.Client{}
		if timeout > 0 {
			c.Timeout = timeout
		}
		return c, "http://" + addr
	}
	sock := ResolveSocketPath()
	return newUnix(func() (net.Conn, error) { return net.Dial("unix", sock) }, timeout), "http://brokerd"
}

// ResolveAddr returns the TCP address for brokerd from the environment or
// config, or "" if neither is set. It does NOT fall back to the unix socket.
func ResolveAddr() string {
	if v := os.Getenv("BROKER_ADDR"); v != "" {
		return v
	}
	if cfg, err := config.Load(config.DefaultPath()); err == nil && cfg.Broker.Addr != "" {
		return cfg.Broker.Addr
	}
	return ""
}

// ResolveSocketPath returns the unix socket path for brokerd, resolving from
// the environment, config file, or the per-uid default.
func ResolveSocketPath() string {
	if v := os.Getenv("BROKER_SOCKET"); v != "" {
		return v
	}
	if cfg, err := config.Load(config.DefaultPath()); err == nil && cfg.Broker.Socket != "" {
		return cfg.Broker.Socket
	}
	return sockpath.Default()
}

// newUnix builds an *http.Client whose Transport always dials via dialFn.
func newUnix(dialFn func() (net.Conn, error), timeout time.Duration) *http.Client {
	c := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return dialFn()
			},
		},
	}
	if timeout > 0 {
		c.Timeout = timeout
	}
	return c
}
