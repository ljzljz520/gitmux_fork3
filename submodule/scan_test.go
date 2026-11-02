package submodule

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitTestEnv is the environment used for all fixture git commands.
func gitTestEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=t",
		"GIT_AUTHOR_EMAIL=t@t.t",
		"GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t.t",
		// Local file:// clones are required to build nested fixtures.
		"GIT_ALLOW_PROTOCOL=file",
	)
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitTestEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// gitCfg runs "git -c <k>=<v>" for commands that need a config override.
func gitCfg(t *testing.T, dir, k, v string, args ...string) {
	t.Helper()
	full := append([]string{"-c", k + "=" + v}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = gitTestEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(full, " "), dir, err, out)
	}
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "init", "-q")
}

func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", name)
	gitCmd(t, dir, "commit", "-q", "-m", msg)
}

func findState(t *testing.T, sum *Summary, p string) *State {
	t.Helper()
	for _, st := range sum.States {
		if st.Path == p {
			return st
		}
	}
	t.Fatalf("submodule %q not found in summary (have %v)", p, paths(sum))
	return nil
}

func paths(sum *Summary) []string {
	var ps []string
	for _, st := range sum.States {
		ps = append(ps, st.Path)
	}
	return ps
}

// buildFixture creates:
//
//	parent/
//	  middle/ (submodule)
//	    leaf/ (nested submodule)
//	  dep/    (submodule)
func buildFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()

	leaf := filepath.Join(root, "leaf")
	initRepo(t, leaf)
	commitFile(t, leaf, "f.txt", "a\n", "l1")
	commitFile(t, leaf, "f.txt", "a\nb\n", "l2")

	middle := filepath.Join(root, "middle")
	initRepo(t, middle)
	gitCfg(t, middle, "protocol.file.allow", "always", "submodule", "add", "-q", leaf, "leaf")
	gitCmd(t, middle, "commit", "-q", "-m", "m1")

	dep := filepath.Join(root, "dep")
	initRepo(t, dep)
	commitFile(t, dep, "d.txt", "d\n", "d1")

	parent := filepath.Join(root, "parent")
	initRepo(t, parent)
	gitCfg(t, parent, "protocol.file.allow", "always", "submodule", "add", "-q", middle, "middle")
	gitCfg(t, parent, "protocol.file.allow", "always", "submodule", "add", "-q", dep, "dep")
	gitCmd(t, parent, "commit", "-q", "-m", "p1")
	// Fixture setup may populate nested submodules; gitmux itself never does.
	gitCfg(t, filepath.Join(parent, "middle"), "protocol.file.allow", "always",
		"submodule", "update", "--init", "leaf")

	return parent
}

func TestScanCleanFixture(t *testing.T) {
	parent := buildFixture(t)
	sum := Scan(context.Background(), parent, Options{MaxDepth: 3, Timeout: 10 * time.Second})
	if sum.Incomplete {
		t.Fatalf("summary should be complete: %v", paths(sum))
	}

	// All registered submodules (including the nested one) are present and
	// none has anything notable.
	for _, p := range []string{"middle", "dep", "middle/leaf"} {
		st := findState(t, sum, p)
		if st.Uninitialized || st.PointerChanged || st.Conflict ||
			st.NumStaged != 0 || st.NumConflicts != 0 || st.NumModified != 0 ||
			st.NumUntracked != 0 || st.Incomplete {
			t.Errorf("%q unexpectedly notable: %#v", p, st)
		}
	}
	if findState(t, sum, "middle").Depth != 1 || findState(t, sum, "middle/leaf").Depth != 2 {
		t.Errorf("unexpected depths: %d, %d", findState(t, sum, "middle").Depth, findState(t, sum, "middle/leaf").Depth)
	}
}

func TestScanInternalChanges(t *testing.T) {
	parent := buildFixture(t)

	// Modified tracked file + untracked file inside dep.
	if err := os.WriteFile(filepath.Join(parent, "dep", "d.txt"), []byte("d\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "dep", "u.new"), []byte("u\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sum := Scan(context.Background(), parent, Options{MaxDepth: 3, Timeout: 10 * time.Second})
	if sum.Incomplete {
		t.Fatalf("unexpected incomplete summary")
	}
	dep := findState(t, sum, "dep")
	if dep.NumModified != 1 || dep.NumUntracked != 1 {
		t.Errorf("dep counts = %#v, want 1 modified / 1 untracked", dep)
	}
	if dep.PointerChanged || dep.Uninitialized {
		t.Errorf("dep flags wrong: %#v", dep)
	}
}

func TestScanUninitialized(t *testing.T) {
	parent := buildFixture(t)
	gitCmd(t, parent, "submodule", "deinit", "-f", "dep")

	sum := Scan(context.Background(), parent, Options{MaxDepth: 3, Timeout: 10 * time.Second})
	if sum.Incomplete {
		t.Fatalf("uninitialized submodule is not an incomplete read")
	}
	dep := findState(t, sum, "dep")
	if !dep.Uninitialized {
		t.Errorf("dep should be uninitialized: %#v", dep)
	}
}

func TestScanPointerChanged(t *testing.T) {
	parent := buildFixture(t)

	// Move middle to a new commit (submodule HEAD diverges from the recorded
	// gitlink).
	commitFile(t, filepath.Join(parent, "middle"), "new.txt", "z\n", "m2")

	sum := Scan(context.Background(), parent, Options{MaxDepth: 3, Timeout: 10 * time.Second})
	mid := findState(t, sum, "middle")
	if !mid.PointerChanged {
		t.Errorf("middle pointer should be flagged changed: %#v", mid)
	}

	// Stage the new pointer in the parent index: still a pointer change.
	gitCmd(t, parent, "add", "middle")
	sum = Scan(context.Background(), parent, Options{MaxDepth: 3, Timeout: 10 * time.Second})
	mid = findState(t, sum, "middle")
	if !mid.PointerChanged {
		t.Errorf("staged middle pointer should be flagged changed: %#v", mid)
	}
}

func TestScanNestedConflict(t *testing.T) {
	parent := buildFixture(t)
	leaf := filepath.Join(parent, "middle", "leaf")

	// A freshly populated submodule is on a detached HEAD; resolve the real
	// local branch instead of relying on the current ref.
	cmd := exec.Command("git", "for-each-ref", "refs/heads", "--format=%(refname:short)")
	cmd.Dir = leaf
	branchBytes, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	branch := strings.TrimSpace(string(branchBytes))
	if branch == "" {
		t.Fatal("no local branch in leaf fixture")
	}

	// Build an add/add merge conflict inside the nested submodule.
	gitCmd(t, leaf, "checkout", "-qb", "cb")
	commitFile(t, leaf, "cf.txt", "side\n", "cb1")
	gitCmd(t, leaf, "checkout", "-q", branch)
	commitFile(t, leaf, "cf.txt", "base\n", "main1")
	// The merge is expected to conflict.
	mc := exec.Command("git", "merge", "--no-ff", "-q", "cb")
	mc.Dir = leaf
	mc.Env = gitTestEnv()
	_ = mc.Run()

	sum := Scan(context.Background(), parent, Options{MaxDepth: 3, Timeout: 10 * time.Second})
	if sum.Incomplete {
		t.Fatalf("conflict state is readable, summary should be complete: %v", paths(sum))
	}
	leafSt := findState(t, sum, "middle/leaf")
	if leafSt.NumConflicts != 1 {
		t.Errorf("middle/leaf conflicts = %d, want 1: %#v", leafSt.NumConflicts, leafSt)
	}
	if leafSt.Uninitialized {
		t.Errorf("middle/leaf should be initialized: %#v", leafSt)
	}

	// A conflicted gitlink at the parent level is reported on the entry too.
	// Simulate unresolved merge stages 1/2/3 in the index, reusing dep's
	// actual recorded SHA (update-index validates object existence).
	rp := exec.Command("git", "rev-parse", ":dep")
	rp.Dir = parent
	depSHABytes, err := rp.Output()
	if err != nil {
		t.Fatal(err)
	}
	depSHA := strings.TrimSpace(string(depSHABytes))
	info := "160000 " + depSHA + " 1\tdep\n" +
		"160000 " + depSHA + " 2\tdep\n" +
		"160000 " + depSHA + " 3\tdep\n"
	c := exec.Command("git", "update-index", "--index-info")
	c.Dir = parent
	c.Env = gitTestEnv()
	c.Stdin = strings.NewReader(info)
	if cout, cerr := c.CombinedOutput(); cerr != nil {
		t.Fatalf("update-index: %v\n%s", cerr, cout)
	}
	sum = Scan(context.Background(), parent, Options{MaxDepth: 3, Timeout: 10 * time.Second})
	if !findState(t, sum, "dep").Conflict {
		t.Errorf("dep gitlink should be flagged conflicted: %#v", findState(t, sum, "dep"))
	}
}

func TestScanDepthLimit(t *testing.T) {
	parent := buildFixture(t)

	sum := Scan(context.Background(), parent, Options{MaxDepth: 1, Timeout: 10 * time.Second})
	if findState(t, sum, "middle") == nil || findState(t, sum, "dep") == nil {
		t.Fatalf("direct submodules missing: %v", paths(sum))
	}
	for _, st := range sum.States {
		if st.Depth > 1 {
			t.Fatalf("depth limit violated: %q depth %d", st.Path, st.Depth)
		}
	}

	// MaxDepth 0 disables scanning.
	if sum := Scan(context.Background(), parent, Options{MaxDepth: 0}); len(sum.States) != 0 || sum.Incomplete {
		t.Fatalf("disabled scan should be empty: %#v", sum)
	}
}

func TestScanTimeoutMarksIncomplete(t *testing.T) {
	parent := buildFixture(t)

	// Deadline already elapsed: the very first command gets canceled.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	sum := Scan(ctx, parent, Options{MaxDepth: 3, Timeout: 0})
	if !sum.Incomplete {
		t.Fatalf("expired context should yield an incomplete summary: %#v", sum)
	}

	// A tiny explicit timeout has the same effect.
	sum = Scan(context.Background(), parent, Options{MaxDepth: 3, Timeout: time.Nanosecond})
	if !sum.Incomplete {
		t.Fatalf("tiny timeout should yield an incomplete summary: %#v", sum)
	}
}

func TestScanUnreadableRoot(t *testing.T) {
	// A directory that is not a git repository: reading the index fails and
	// the summary must be flagged incomplete rather than erroring out.
	sum := Scan(context.Background(), t.TempDir(), Options{MaxDepth: 3, Timeout: 10 * time.Second})
	if !sum.Incomplete {
		t.Fatalf("non-git root should yield an incomplete summary: %#v", sum)
	}
}
