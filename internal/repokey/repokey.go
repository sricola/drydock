// Package repokey canonicalizes git repo references into a stable
// "host/owner/repo" key so operator config (verify.repos keys) matches
// however a task happens to spell the same repository (https vs scp-style
// vs ssh://, credentials, .git suffix, host case).
package repokey

import (
	"net/url"
	"strings"
)

// Normalize returns the canonical key for a repo reference. Unrecognized
// shapes come back trimmed but otherwise unchanged — matching then simply
// fails, which is the safe direction (no verification configured).
func Normalize(ref string) string {
	ref = strings.TrimSpace(ref)
	// scp-style: git@host:owner/repo(.git)
	if at := strings.Index(ref, "@"); at >= 0 && !strings.Contains(ref, "://") {
		if colon := strings.Index(ref[at:], ":"); colon > 0 {
			host := ref[at+1 : at+colon]
			path := ref[at+colon+1:]
			return join(host, path)
		}
	}
	if strings.Contains(ref, "://") {
		if u, err := url.Parse(ref); err == nil && u.Host != "" {
			return join(u.Host, u.Path)
		}
		return ref
	}
	// already host/path shaped
	if i := strings.IndexByte(ref, '/'); i > 0 {
		return join(ref[:i], ref[i+1:])
	}
	return ref
}

func join(host, path string) string {
	host = strings.ToLower(host)
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" {
		return host
	}
	return host + "/" + path
}
