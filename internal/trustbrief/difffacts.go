// Package trustbrief assembles broker-observed evidence about a completed
// task: what the review diff structurally contains, what policy bounded the
// run, and what was spent. Every fact is computed host-side from host-git
// output and broker state; nothing here is derived from agent claims — that
// separation is the point of the artifact.
package trustbrief

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Diff line prefixes/suffixes the classifier dispatches on, as package-level
// byte slices so Analyze can match against the raw ReadLine buffer without
// allocating a string per line (a 32 MiB diff is ~500k lines).
var (
	bpDiffGit       = []byte("diff --git ")
	bpHunk          = []byte("@@")
	bpNewFileMode   = []byte("new file mode ")
	bpNewMode       = []byte("new mode ")
	bpIndex         = []byte("index ")
	bpRenameFrom    = []byte("rename from ")
	bpBinaryFiles   = []byte("Binary files ")
	bpPlusPlus      = []byte("+++")
	bpMinusMinus    = []byte("---")
	bpTruncated     = []byte("... [diff truncated at ")
	bsDiffer        = []byte(" differ")
	bsSymlinkMode   = []byte(" 120000")
	bGitBinaryPatch = []byte("GIT binary patch")
	bModeSymlink    = []byte("120000")
	bModeExec       = []byte("100755")
)

// FileChange is one changed file with its hunk-line counts.
type FileChange struct {
	Path string `json:"path"`
	Adds int    `json:"adds"`
	Dels int    `json:"dels"`
}

// Flag marks a structural risk pattern a reviewer should look at first.
// Kinds are stable identifiers (they will later drive the acknowledgment
// flow); Paths are capped examples, not an exhaustive list.
type Flag struct {
	Kind  string   `json:"kind"`
	Paths []string `json:"paths"`
}

// Flag kinds. Stable strings: they are persisted in brief.json artifacts.
const (
	FlagBinary     = "binary-changed"
	FlagSymlink    = "symlink"
	FlagExecBit    = "exec-bit"
	FlagDependency = "dependency-manifest"
	FlagLockfile   = "lockfile"
	FlagCIWorkflow = "ci-workflow"
	FlagGitMeta    = "git-metadata"
)

// DiffFacts is the broker-computed structural summary of the review diff.
type DiffFacts struct {
	SHA256       string       `json:"sha256"`
	Bytes        int          `json:"bytes"`
	Truncated    bool         `json:"truncated"`
	Files        []FileChange `json:"files"`
	FilesOmitted int          `json:"files_omitted,omitempty"`
	Flags        []Flag       `json:"flags"`
}

// Bounds. The diff body is attacker-controlled: a hostile task can emit any
// bytes it likes into tracked files. Everything the parser retains is capped
// so a crafted diff cannot balloon the Brief or the broker's memory.
const (
	maxTrackedFiles = 4096 // FileChange entries retained; the rest are counted
	maxPathBytes    = 512  // stored path length
	maxFlagPaths    = 32   // example paths retained per flag kind
	maxHeaderLine   = 4096 // git header lines are short; longer prefixes are content
)

// Analyze computes DiffFacts from a unified diff produced by the host-side
// `git diff --cached` (stage.CaptureDiff). The diff FRAMING (headers, mode
// lines) is trusted git output; the CONTENT lines are attacker data and are
// only ever counted, never interpreted.
func Analyze(diff string) DiffFacts {
	sum := sha256.Sum256([]byte(diff))
	facts := DiffFacts{SHA256: hex.EncodeToString(sum[:]), Bytes: len(diff), Files: []FileChange{}, Flags: []Flag{}}

	flagged := map[string][]string{} // kind -> capped example paths, deduped by linear scan
	addFlag := func(kind, p string) {
		if p == "" {
			p = "(unknown path)"
		}
		got := flagged[kind]
		if len(got) >= maxFlagPaths {
			return
		}
		for _, q := range got {
			if q == p {
				return
			}
		}
		flagged[kind] = append(flagged[kind], p)
	}

	r := bufio.NewReaderSize(strings.NewReader(diff), 64<<10)
	var cur *FileChange
	curPath := ""
	inHunk := false
	for {
		line, err := boundedLine(r)
		if len(line) == 0 && err != nil {
			break
		}
		switch {
		case bytes.HasPrefix(line, bpDiffGit):
			curPath = capPath(parseGitHeaderPath(string(line)))
			inHunk = false
			if len(facts.Files) < maxTrackedFiles {
				facts.Files = append(facts.Files, FileChange{Path: curPath})
				cur = &facts.Files[len(facts.Files)-1]
			} else {
				facts.FilesOmitted++
				cur = nil
			}
			classifyPath(curPath, addFlag)
		case bytes.HasPrefix(line, bpHunk):
			inHunk = true
		case bytes.HasPrefix(line, bpNewFileMode), bytes.HasPrefix(line, bpNewMode):
			mode := line[bytes.LastIndexByte(line, ' ')+1:]
			if bytes.HasPrefix(mode, bModeSymlink) {
				addFlag(FlagSymlink, curPath)
			}
			if bytes.HasPrefix(mode, bModeExec) {
				addFlag(FlagExecBit, curPath)
			}
		case !inHunk && bytes.HasPrefix(line, bpIndex):
			// `index aaa..bbb 120000` (no mode lines) is what git emits when an
			// EXISTING symlink's target changes — new/old mode lines only appear
			// when the mode itself changes. Without this, retargeting a tracked
			// symlink (e.g. to /etc/passwd) goes unflagged. Deliberately only the
			// symlink mode is checked here: 100644/100755 on an index line means
			// an existing file's content changed (with its exec bit unchanged),
			// which is normal and must NOT re-trigger FlagExecBit (that flag means
			// the bit was ADDED, which "new mode "/"new file mode " already catch).
			if bytes.HasSuffix(line, bsSymlinkMode) {
				addFlag(FlagSymlink, curPath)
			}
		case !inHunk && bytes.HasPrefix(line, bpRenameFrom):
			// A pure rename (`similarity index 100%` / rename from / rename to,
			// no hunks) only classifies the b-path via the "diff --git" header
			// above. That misses a rename that moves a flagged path OUT of its
			// significant location (e.g. `git mv .github/workflows/ci.yml
			// ci.yml.disabled` silently disables CI) since the origin path is
			// never otherwise classified. Classify the origin path too.
			//
			// Git quotes this path (core.quotepath default) whenever it contains
			// non-ASCII bytes or a space, exactly like the "diff --git" b-path
			// above — `rename from ".github/workflows/ci\302\251.yml"`. Without
			// unquoting, every prefix/base check in classifyPath fails against
			// the literal quoted+escaped string and the rename-out goes unflagged.
			classifyPath(capPath(unquotePath(string(bytes.TrimPrefix(line, bpRenameFrom)))), addFlag)
		case bytes.HasPrefix(line, bpBinaryFiles) && bytes.HasSuffix(line, bsDiffer),
			bytes.Equal(line, bGitBinaryPatch):
			addFlag(FlagBinary, curPath)
		case !inHunk && (bytes.HasPrefix(line, bpPlusPlus) || bytes.HasPrefix(line, bpMinusMinus)):
			// hunk file headers, not content
		case inHunk && len(line) > 0 && line[0] == '+':
			if cur != nil {
				cur.Adds++
			}
		case inHunk && len(line) > 0 && line[0] == '-':
			if cur != nil {
				cur.Dels++
			}
		case bytes.HasPrefix(line, bpTruncated):
			// The exact marker stage.gitDiffCapped appends when the review
			// diff exceeded its cap. The committed change is complete; the
			// reviewer must know this summary is not.
			facts.Truncated = true
		}
		if err != nil {
			break
		}
	}

	kinds := make([]string, 0, len(flagged))
	for k := range flagged {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		facts.Flags = append(facts.Flags, Flag{Kind: k, Paths: flagged[k]})
	}
	return facts
}

// boundedLine reads one line, retaining at most maxHeaderLine bytes and
// discarding the remainder. Every line the parser *interprets* (git headers,
// mode lines) is short; content lines only need their first byte for +/-
// counting, so dropping long tails loses nothing and keeps memory bounded.
// The returned slice is valid only until the next boundedLine call (it aliases
// bufio's internal buffer in the common case); callers must string()-copy any
// bytes they retain. Avoiding a per-line copy is the whole point.
func boundedLine(r *bufio.Reader) ([]byte, error) {
	frag, isPrefix, err := r.ReadLine()
	if !isPrefix {
		// Whole line fit the reader buffer: frag is stable until the next read
		// (which the caller does next iteration, after using this line). No copy.
		if len(frag) > maxHeaderLine {
			return frag[:maxHeaderLine], err
		}
		return frag, err
	}
	// Over-long line (exceeds the reader buffer): copy the bounded prefix before
	// draining the rest, since draining reuses frag's backing array.
	n := len(frag)
	if n > maxHeaderLine {
		n = maxHeaderLine
	}
	line := append([]byte(nil), frag[:n]...)
	for isPrefix && err == nil {
		_, isPrefix, err = r.ReadLine() // discard: only the prefix is interpreted
	}
	return line, err
}

// parseGitHeaderPath extracts the post-change (b/) path from a
// `diff --git a/X b/Y` header. Git quotes paths containing spaces or
// non-ASCII (`"b/we ird"`); those are unquoted with strconv. A pathological
// a-path that itself contains ` "b/` or ` b/` can misattribute the path —
// that degrades a flag's example path, never the parse (advisory evidence,
// not an enforcement input).
func parseGitHeaderPath(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if i := strings.Index(rest, ` "b/`); i >= 0 {
		if unq, err := strconv.Unquote(rest[i+1:]); err == nil {
			return strings.TrimPrefix(unq, "b/")
		}
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return ""
}

// unquotePath undoes git's core.quotepath escaping: a path containing
// non-ASCII bytes or a space is emitted as a C-style quoted string with
// octal escapes (e.g. `".github/workflows/ci\302\251.yml"`). If s isn't a
// quoted string, or fails to unquote, it is returned unchanged — callers
// that receive plain ASCII paths (the common case) are unaffected.
func unquotePath(s string) string {
	if strings.HasPrefix(s, `"`) {
		if unq, err := strconv.Unquote(s); err == nil {
			return unq
		}
	}
	return s
}

func capPath(p string) string {
	if len(p) > maxPathBytes {
		return p[:maxPathBytes]
	}
	return p
}

// classifyPath adds path-based flags. The lists are deliberately small and
// high-signal: they mark files where a malicious change has outsized blast
// radius (CI executes on the host org's runners; manifests/lockfiles pull
// remote code; git metadata alters host git behavior).
func classifyPath(p string, add func(kind, path string)) {
	if p == "" {
		return
	}
	base := path.Base(p)
	switch {
	case strings.HasPrefix(p, ".github/workflows/"),
		base == ".gitlab-ci.yml", base == "Jenkinsfile",
		strings.HasPrefix(p, ".circleci/"), base == "azure-pipelines.yml":
		add(FlagCIWorkflow, p)
	}
	switch base {
	case "package.json", "go.mod", "requirements.txt", "pyproject.toml",
		"Cargo.toml", "Gemfile", "pom.xml", "build.gradle", "build.gradle.kts":
		add(FlagDependency, p)
	case "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum",
		"Cargo.lock", "Gemfile.lock", "poetry.lock", "uv.lock", "composer.lock":
		add(FlagLockfile, p)
	case ".gitattributes", ".gitmodules":
		add(FlagGitMeta, p)
	}
	if strings.HasPrefix(p, ".githooks/") {
		add(FlagGitMeta, p)
	}
}
