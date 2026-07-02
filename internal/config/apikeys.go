package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"drydock/internal/provider"
)

// knownAPIKeys are the only env names the store recognizes. Derived from the
// provider registry (rows with a non-empty APIKeyEnv) so a new provider
// automatically gains a managed key slot without editing this file.
var knownAPIKeys = func() []string {
	var keys []string
	for _, p := range provider.Registry {
		if p.APIKeyEnv != "" {
			keys = append(keys, p.APIKeyEnv)
		}
	}
	return keys
}()

// isKnownAPIKey reports whether name is one the store manages.
func isKnownAPIKey(name string) bool {
	for _, k := range knownAPIKeys {
		if k == name {
			return true
		}
	}
	return false
}

// APIKeysPath is the host-only api-key store — the api_key-mode peer of the
// OAuth json files. Mode 0600; read host-side, never enters the VM. Returns ""
// when the home directory is unresolvable.
func APIKeysPath() string {
	d := Dir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "api-keys.env")
}

// LoadAPIKeys reads KEY=value lines from path. Blank lines and # comments are
// ignored, as are any keys outside knownAPIKeys — load and WriteAPIKey agree on
// the managed key set, so an unrecognized line can't be loaded one moment and
// silently dropped on the next rewrite. A missing or unreadable file yields an
// empty map and nil error (the store is optional). A scanner error (e.g. a line
// exceeding bufio.MaxScanTokenSize) returns the keys scanned so far and the error.
func LoadAPIKeys(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out, nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if !isKnownAPIKey(k) {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// WriteAPIKey upserts key=value in the store at path, preserving the other
// recognized key. 0600, atomic temp+rename; the parent dir is created 0700.
func WriteAPIKey(path, key, value string) error {
	keys, _ := LoadAPIKeys(path)
	keys[key] = value
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# drydock API keys — host-only, 0600. Never enters the sandbox VM.\n")
	for _, k := range knownAPIKeys {
		if v := keys[k]; v != "" {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
