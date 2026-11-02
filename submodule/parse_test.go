package submodule

import (
	"testing"
)

func TestParseStage(t *testing.T) {
	// Output of `git ls-files --stage -z`, including regular files, a
	// stage-0 gitlink and its merge stages 1/2/3.
	in := []byte("100644 8eae3f5fcf94b6c84c4c295aad000124f4d1b836 0\t.gitmodules\x00" +
		"160000 16cbf0e2ffcf37fea02bb2e00c1a31d9b9cc6d72 0\tmiddle\x00" +
		"160000 16cbf0e2ffcf37fea02bb2e00c1a31d9b9cc6d72 1\tmiddle\x00" +
		"160000 facc2ef44a9a6d3c0841c7dd3a404b1f91776229 2\tmiddle\x00" +
		"160000 234bf211a1a5452feb5dbffac8c5f5991ea98496 3\tmiddle\x00" +
		"160000 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 0\tother\x00")

	links := parseStage(in)
	if len(links) != 2 {
		t.Fatalf("got %d gitlinks, want 2: %#v", len(links), links)
	}

	mid := links["middle"]
	if mid.indexSHA != "16cbf0e2ffcf37fea02bb2e00c1a31d9b9cc6d72" {
		t.Errorf("middle index SHA = %q", mid.indexSHA)
	}
	if !mid.conflict {
		t.Errorf("middle should be flagged as conflicted (stages 1/2/3)")
	}

	other := links["other"]
	if other.indexSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || other.conflict {
		t.Errorf("other = %#v, want stage-0 only", other)
	}
}

func TestParseStageUninitializedStillEnumerated(t *testing.T) {
	// An uninitialized submodule is absent from `git status` but still
	// present as a stage-0 gitlink in the index.
	in := []byte("160000 16cbf0e2ffcf37fea02bb2e00c1a31d9b9cc6d72 0\tdep\x00")
	links := parseStage(in)
	dep, ok := links["dep"]
	if !ok {
		t.Fatalf("uninitialized gitlink missing")
	}
	if dep.indexSHA == "" || dep.conflict {
		t.Errorf("dep = %#v", dep)
	}
}

func TestParseTree(t *testing.T) {
	in := []byte("100644 blob 8eae3f5fcf94b6c84c4c295aad000124f4d1b836\t.gitmodules\x00" +
		"160000 commit 16cbf0e2ffcf37fea02bb2e00c1a31d9b9cc6d72\tmiddle\x00")
	heads := parseTree(in)
	if len(heads) != 1 {
		t.Fatalf("got %d entries, want 1: %#v", len(heads), heads)
	}
	if heads["middle"] != "16cbf0e2ffcf37fea02bb2e00c1a31d9b9cc6d72" {
		t.Errorf("middle = %q", heads["middle"])
	}
}

func TestParsePorcelainV2(t *testing.T) {
	// Representative porcelain v2 stream: header, pointer-moved dirty
	// submodule entry (ignored here: internal files only are counted),
	// staged file, unstaged file, rename, conflict and untracked file.
	in := []byte(
		"# branch.oid facc2ef44a9a6d3c0841c7dd3a404b1f91776229\x00" +
			"# branch.head main\x00" +
			"1 A. N... 000000 100644 100644 0000000000000000000000000000000000000000 1111111111111111111111111111111111111111 added.txt\x00" +
			"1 .M N... 100644 100644 100644 2222222222222222222222222222222222222222 2222222222222222222222222222222222222222 mod.txt\x00" +
			"1 M. N... 100644 100644 100644 3333333333333333333333333333333333333333 3333333333333333333333333333333333333333 staged.txt\x00" +
			"2 R. N... 100644 100644 100644 abc abc R100 old.txt\x00new.txt\x00" +
			"u AA N... 000000 100644 100644 100644 0000000000000000000000000000000000000000 d1 d2 conflict.txt\x00" +
			"? untracked.txt\x00" +
			"! ignored.txt\x00")

	head, staged, modified, untracked, conflicts := parsePorcelainV2(in)
	if head != "facc2ef44a9a6d3c0841c7dd3a404b1f91776229" {
		t.Errorf("head = %q", head)
	}
	// A. added, M. staged, R. renamed-in-index -> 3 staged.
	if staged != 3 {
		t.Errorf("staged = %d, want 3", staged)
	}
	// .M -> 1 modified.
	if modified != 1 {
		t.Errorf("modified = %d, want 1", modified)
	}
	if untracked != 1 {
		t.Errorf("untracked = %d, want 1", untracked)
	}
	if conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", conflicts)
	}
}

func TestParsePorcelainV2UnbornAndEmpty(t *testing.T) {
	if head, s, m, u, c := parsePorcelainV2([]byte("# branch.oid (initial)\x00")); head != "" || s+m+u+c != 0 {
		t.Errorf("unborn branch parsed wrong: %q %d %d %d %d", head, s, m, u, c)
	}
	if head, s, m, u, c := parsePorcelainV2(nil); head != "" || s+m+u+c != 0 {
		t.Errorf("empty stream parsed wrong: %q %d %d %d %d", head, s, m, u, c)
	}
}
