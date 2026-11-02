package tmux

import (
	"testing"

	"github.com/arl/gitmux/submodule"
)

func subTestConfig() Config {
	return Config{
		Symbols: symbols{
			Submodules:          "[sym:subs]",
			SubmodulePointer:    "[sym:ptr]",
			SubmoduleUninit:     "[sym:uninit]",
			SubmoduleIncomplete: "[sym:inc]",
			Staged:              "[sym:staged]",
			Conflict:            "[sym:conf]",
			Modified:            "[sym:mod]",
			Untracked:           "[sym:untracked]",
		},
		Styles: styles{
			Clear:      "[clear]",
			Submodules: "[style:subs]",
			Staged:     "[style:staged]",
			Conflict:   "[style:conf]",
			Modified:   "[style:mod]",
			Untracked:  "[style:untracked]",
		},
	}
}

func TestSubmodules(t *testing.T) {
	tests := []struct {
		name string
		sum  *submodule.Summary
		opts options
		want string
	}{
		{
			name: "nil summary",
			sum:  nil,
			want: "",
		},
		{
			name: "all clean hidden",
			sum: &submodule.Summary{
				States: []*submodule.State{
					{Path: "a", Depth: 1},
				},
			},
			want: "",
		},
		{
			name: "uninitialized entry",
			sum: &submodule.Summary{
				States: []*submodule.State{
					{Path: "dep", Depth: 1, Uninitialized: true},
				},
			},
			want: "[clear][style:subs][sym:subs][clear][style:subs]dep [clear][style:subs][sym:uninit]",
		},
		{
			name: "pointer and internal counts",
			sum: &submodule.Summary{
				States: []*submodule.State{
					{Path: "mid", Depth: 1, PointerChanged: true, NumStaged: 1, NumModified: 2, NumUntracked: 3},
				},
			},
			want: "[clear][style:subs][sym:subs]" +
				"[clear][style:subs]mid [clear][style:subs][sym:ptr]" +
				" [style:staged][sym:staged]1 [style:mod][sym:mod]2 [style:untracked][sym:untracked]3",
		},
		{
			name: "internal conflict plus conflicted gitlink",
			sum: &submodule.Summary{
				States: []*submodule.State{
					{Path: "mid", Depth: 1, Conflict: true, NumConflicts: 1},
				},
			},
			// Gitlink conflict adds one to the conflict count.
			want: "[clear][style:subs][sym:subs]" +
				"[clear][style:subs]mid [style:conf][sym:conf]2",
		},
		{
			name: "two entries joined and incomplete entry",
			sum: &submodule.Summary{
				States: []*submodule.State{
					{Path: "a", Depth: 1, NumModified: 1},
					{Path: "b", Depth: 1, Incomplete: true},
				},
			},
			want: "[clear][style:subs][sym:subs]" +
				"[clear][style:subs]a [style:mod][sym:mod]1" +
				"[clear][style:subs], [clear][style:subs]b [clear][style:subs][sym:inc]",
		},
		{
			name: "globally incomplete summary gets marker",
			sum:  &submodule.Summary{Incomplete: true},
			want: "[clear][style:subs][sym:subs][sym:inc]",
		},
		{
			name: "max shown hides extra entries",
			opts: options{SubmoduleMaxShown: 1},
			sum: &submodule.Summary{
				States: []*submodule.State{
					{Path: "a", Depth: 1, NumModified: 1},
					{Path: "b", Depth: 1, Uninitialized: true},
					{Path: "c", Depth: 1, Uninitialized: true},
				},
			},
			want: "[clear][style:subs][sym:subs]" +
				"[clear][style:subs]a [style:mod][sym:mod]1" +
				"[clear][style:subs], +2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := subTestConfig()
			cfg.Options = tt.opts
			f := &Formater{Config: cfg, Submodules: tt.sum}
			compareStrings(t, tt.want, f.submodules())
		})
	}
}

func TestSubmodulesInLayout(t *testing.T) {
	cfg := subTestConfig()
	cfg.Layout = []string{"submodules"}
	cfg.Options = options{}
	sum := &submodule.Summary{
		States: []*submodule.State{
			{Path: "dep", Uninitialized: true},
		},
	}
	f := &Formater{Config: cfg, Submodules: sum}
	got := f.format()
	want := "[clear][style:subs][sym:subs][clear][style:subs]dep [clear][style:subs][sym:uninit]" +
		resetStyles
	compareStrings(t, want, got)
}
