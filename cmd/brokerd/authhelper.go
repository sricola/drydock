package main

import (
	"bufio"
	"crypto/subtle"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// runSquidAuthHelper implements squid's basic-auth helper protocol: read one
// "<user> <pass>" line at a time, answer "OK" or "ERR". squid URL-encodes both
// fields. The token file (lines of "<user> <secret>") is re-read per request so
// the broker can add/remove credentials live without restarting the helper.
func runSquidAuthHelper(tokenPath string, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		user, pass := parseHelperLine(sc.Text())
		// Constant-time compare on the per-task proxy secret, matching the
		// gateway bearer check. An absent user yields "" — reject rather than
		// letting an empty stored secret match an empty password.
		secret := lookupToken(tokenPath, user)
		if user != "" && secret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(pass)) == 1 {
			fmt.Fprintln(out, "OK")
		} else {
			fmt.Fprintln(out, "ERR")
		}
	}
	return sc.Err()
}

func parseHelperLine(line string) (user, pass string) {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) < 2 {
		return "", ""
	}
	u, _ := url.QueryUnescape(f[0])
	p, _ := url.QueryUnescape(f[1])
	return u, p
}

// lookupToken returns the secret registered for user, or "" if absent.
func lookupToken(tokenPath, user string) string {
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) == 2 && f[0] == user {
			return f[1]
		}
	}
	return ""
}
