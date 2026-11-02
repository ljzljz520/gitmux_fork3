// Package submodule scans the nested submodules of a Git repository and
// summarizes their state.
//
// The summary distinguishes submodule pointer changes (the checked-out commit
// differs from the gitlink recorded in the superproject, or the gitlink is
// staged), internal modifications (staged/modified/untracked files or
// conflicts inside the submodule working tree), gitlink conflicts in the
// superproject index, and uninitialized submodules.
//
// The scan never initializes a submodule and never accesses the network: it
// only runs read-only porcelain commands. Traversal is bounded by a maximum
// depth and a total timeout. Whenever the state of a submodule cannot be read
// completely, the corresponding entry (and the summary as a whole) is flagged
// as incomplete.
package submodule

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options controls the nested submodule scan.
type Options struct {
	// MaxDepth is the maximum submodule depth scanned below the parent
	// repository. Direct submodules have depth 1. A value <= 0 disables
	// scanning entirely.
	MaxDepth int

	// Timeout is the total time budget for the whole traversal. The parent
	// context deadline (if any) still applies. A value <= 0 means no
	// additional timeout.
	Timeout time.Duration
}

// State is the summarized state of a single submodule.
type State struct {
	// Path is the path of the submodule relative to the parent repository,
	// using slash separators (for example "vendor/lib" or "outer/inner").
	Path string

	// Depth is the nesting depth below the parent repository (1 for direct
	// submodules).
	Depth int

	// Uninitialized reports that the submodule is not checked out (its
	// working directory has no .git entry). Such submodules are never
	// descended into.
	Uninitialized bool

	// PointerChanged reports that the submodule pointer changed: either the
	// superproject index records a different gitlink than HEAD (staged
	// pointer change / newly added submodule), or the submodule HEAD is at a
	// different commit than the recorded gitlink.
	PointerChanged bool

	// Conflict reports that the gitlink itself is unmerged in the
	// superproject index (merge stages 1/2/3 are present).
	Conflict bool

	// NumStaged is the number of staged changes inside the submodule.
	NumStaged int
	// NumConflicts is the number of unmerged paths inside the submodule.
	NumConflicts int
	// NumModified is the number of unstaged modifications inside the
	// submodule.
	NumModified int
	// NumUntracked is the number of untracked paths inside the submodule.
	NumUntracked int

	// Incomplete reports that the state of this submodule could not be read
	// completely (a git command failed or the traversal timed out).
	Incomplete bool
}

// Summary is the result of a nested submodule scan.
type Summary struct {
	// States contains the scanned submodules, in traversal (depth-first)
	// order. Every submodule registered in the superproject index is
	// present, including clean and uninitialized ones.
	States []*State

	// Incomplete reports that the summary itself is incomplete: the
	// traversal timed out, was canceled, or a git command failed.
	Incomplete bool
}

// Scan inspects the nested submodules of the repository at root and returns
// their summary. Scan never returns an error: a repository that cannot be read
// yields an incomplete summary.
func Scan(ctx context.Context, root string, opts Options) *Summary {
	sum := &Summary{}
	if opts.MaxDepth <= 0 {
		return sum
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	root, err := filepath.Abs(root)
	if err != nil {
		sum.Incomplete = true
		return sum
	}

	s := &scanner{ctx: ctx, maxDepth: opts.MaxDepth}
	s.walk(root, "", 1)
	sum.States = s.states
	sum.Incomplete = s.incomplete
	return sum
}

type scanner struct {
	ctx        context.Context
	maxDepth   int
	states     []*State
	incomplete bool
}

// gitlink is a submodule entry (mode 160000) in a repository index.
type gitlink struct {
	indexSHA string // stage-0 SHA, empty if there is no stage-0 entry
	conflict bool   // merge stages 1/2/3 are present
}

func (s *scanner) walk(repoDir, prefix string, depth int) {
	if s.ctx.Err() != nil {
		s.incomplete = true
		return
	}

	links, err := listGitlinks(s.ctx, repoDir)
	if err != nil {
		// Unable to read the index: the submodules of this repository are
		// unknown, the summary cannot be complete.
		s.incomplete = true
		return
	}
	if len(links) == 0 {
		return
	}

	// Best-effort gitlinks recorded in HEAD (used to detect staged pointer
	// changes).
	heads, err := listHeadGitlinks(s.ctx, repoDir, links)
	if err != nil {
		s.incomplete = true
	}

	names := make([]string, 0, len(links))
	for name := range links {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if s.ctx.Err() != nil {
			s.incomplete = true
			return
		}

		link := links[name]
		full := filepath.Join(repoDir, filepath.FromSlash(name))
		disp := name
		if prefix != "" {
			disp = path.Join(prefix, name)
		}

		st := &State{
			Path:          disp,
			Depth:         depth,
			Uninitialized: !isCheckedOut(full),
			Conflict:      link.conflict,
		}

		// Staged pointer change: index gitlink differs from the gitlink
		// recorded in HEAD (or the gitlink is not in HEAD yet).
		if link.indexSHA != "" {
			if headSHA, ok := heads[name]; !ok || headSHA != link.indexSHA {
				st.PointerChanged = true
			}
		}

		readOK := false
		if !st.Uninitialized {
			head, staged, modified, untracked, conflicts, ok := inspect(s.ctx, full)
			if ok {
				readOK = true
				st.NumStaged = staged
				st.NumModified = modified
				st.NumUntracked = untracked
				st.NumConflicts = conflicts
				if link.indexSHA != "" && head != "" && head != link.indexSHA {
					// Submodule HEAD is at a different commit than the
					// gitlink recorded in the superproject.
					st.PointerChanged = true
				}
			} else {
				st.Incomplete = true
				s.incomplete = true
			}
		}

		s.states = append(s.states, st)

		// Recurse into initialized submodules only, and only when the
		// current submodule state could be read.
		if readOK && depth < s.maxDepth {
			s.walk(full, disp, depth+1)
		}
	}
}

// listGitlinks enumerates submodule gitlinks (mode 160000) of the repository
// index. Using the index instead of "git status" guarantees that
// uninitialized and ignored submodules are enumerated as well.
func listGitlinks(ctx context.Context, dir string) (map[string]gitlink, error) {
	out, err := runGit(ctx, dir, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	return parseStage(out), nil
}

// parseStage parses `git ls-files --stage -z` output and keeps only gitlink
// entries (mode 160000).
func parseStage(b []byte) map[string]gitlink {
	links := map[string]gitlink{}
	for _, rec := range splitNul(b) {
		header, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Split(header, " ")
		// Format: <mode> <sha> <stage>
		if len(fields) != 3 || fields[0] != "160000" {
			continue
		}

		link := links[p]
		switch fields[2] {
		case "0":
			link.indexSHA = fields[1]
		default:
			link.conflict = true
		}
		links[p] = link
	}
	return links
}

// listHeadGitlinks returns the gitlink SHAs recorded in HEAD, keyed by path.
// A repository without commits (unborn HEAD) yields an empty map and no error.
func listHeadGitlinks(ctx context.Context, dir string, links map[string]gitlink) (map[string]string, error) {
	names := make([]string, 0, len(links))
	for name := range links {
		names = append(names, name)
	}
	sort.Strings(names)

	args := append([]string{"ls-tree", "-z", "HEAD", "--"}, names...)
	out, err := runGit(ctx, dir, args...)
	if err != nil {
		if isUnbornHEAD(err) {
			return map[string]string{}, nil
		}
		return map[string]string{}, err
	}

	return parseTree(out), nil
}

// parseTree parses `git ls-tree -z` output and keeps only commit entries
// (gitlinks), keyed by path.
func parseTree(b []byte) map[string]string {
	heads := map[string]string{}
	for _, rec := range splitNul(b) {
		header, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(header)
		// Format: <mode> <type> <sha>
		if len(fields) == 3 && fields[0] == "160000" && fields[1] == "commit" {
			heads[p] = fields[2]
		}
	}
	return heads
}

// inspect runs porcelain v2 status inside an initialized submodule and returns
// its current HEAD and internal change counts. ok is false if the state could
// not be read (command error, cancellation or timeout).
func inspect(ctx context.Context, dir string) (head string, staged, modified, untracked, conflicts int, ok bool) {
	out, err := runGit(ctx, dir,
		"status", "--porcelain=v2", "-z", "--branch",
		"--ignore-submodules=none", "--untracked-files=normal",
	)
	if err != nil {
		return "", 0, 0, 0, 0, false
	}

	head, staged, modified, untracked, conflicts = parsePorcelainV2(out)
	return head, staged, modified, untracked, conflicts, true
}

// parsePorcelainV2 parses `git status --porcelain=v2 -z --branch` output and
// returns the current HEAD and internal change counts.
func parsePorcelainV2(b []byte) (head string, staged, modified, untracked, conflicts int) {
	for _, line := range splitNul(b) {
		switch {
		case strings.HasPrefix(line, "# branch.oid "):
			oid := strings.TrimPrefix(line, "# branch.oid ")
			// On an unborn branch git reports the literal "(initial)".
			if oid != "" && oid != "(initial)" {
				head = oid
			}
		case strings.HasPrefix(line, "u "):
			// Unmerged entry (conflict).
			conflicts++
		case strings.HasPrefix(line, "? "):
			untracked++
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "):
			// Ordinary or renamed entry: "1 <XY> ..." / "2 <XY> ...". In
			// porcelain v2, an unchanged XY position is '.' (not a space).
			fields := strings.SplitN(line, " ", 3)
			if len(fields) >= 2 && len(fields[1]) == 2 {
				switch fields[1][0] {
				case ' ', '.':
				default:
					staged++
				}
				switch fields[1][1] {
				case ' ', '.':
				default:
					modified++
				}
			}
		}
	}
	return head, staged, modified, untracked, conflicts
}

// isCheckedOut reports whether the submodule working tree at p is initialized.
// Initialized submodules have a .git file (embedded git directory) or a .git
// directory. No git command is needed, so uninitialized submodules don't
// trigger any initialization or network access.
func isCheckedOut(p string) bool {
	_, err := os.Stat(filepath.Join(p, ".git"))
	return err == nil
}

func isUnbornHEAD(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown revision") ||
		strings.Contains(msg, "bad revision") ||
		strings.Contains(msg, "not a valid object") ||
		strings.Contains(msg, "ambiguous argument 'head'")
}

// gitError is the error returned by runGit, carrying git stderr.
type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	msg := "git " + strings.Join(e.args, " ") + ": " + e.err.Error()
	if e.stderr != "" {
		msg += ": " + strings.TrimSpace(e.stderr)
	}
	return msg
}

func (e *gitError) Unwrap() error { return e.err }

// runGit runs a read-only git command in directory dir. Commands are run with
// the context so a deadline/timeout cancels them, with optional locking and
// prompts disabled: the scan never writes to the repository, never initializes
// a submodule and never accesses the network.
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	fullArgs := append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, &gitError{args: args, stderr: stderr.String(), err: err}
	}
	return out, nil
}

// splitNul splits NUL-separated git -z output, dropping the trailing empty
// record.
func splitNul(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := strings.Split(string(b), "\x00")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
