package tmux

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arl/gitstatus"
	"gopkg.in/yaml.v3"

	"github.com/arl/gitmux/submodule"
)

// Config is the configuration of the Git status tmux formatter.
type Config struct {
	// Symbols contains the symbols printed before the Git status components.
	Symbols symbols
	// Styles contains the tmux style strings for symbols and Git status
	// components.
	Styles styles
	// Layout sets the output format of the Git status.
	Layout []string `yaml:",flow"`
	// Options contains additional configuration options.
	Options options
}

type symbols struct {
	Branch     string // Branch is the string shown before local branch name.
	HashPrefix string // HasPrefix is the string shown before a SHA1 ref.

	Ahead  string // Ahead is the string shown before the ahead count for the local/upstream branch divergence.
	Behind string // Behind is the string shown before the behind count for the local/upstream branch divergence.

	Staged    string // Staged is the string shown before the count of staged files.
	Conflict  string // Conflict is the string shown before the count of files with conflicts.
	Modified  string // Modified is the string shown before the count of modified files.
	Untracked string // Untracked is the string shown before the count of untracked files.
	Stashed   string // Stashed is the string shown before the count of stash entries.
	Clean     string // Clean is the string shown when the working tree is clean.

	Insertions string // Insertions is the string shown before the count of inserted lines.
	Deletions  string // Deletions is the string shown before the count of deleted lines.

	Submodules          string // Submodules is the string shown before the nested submodule summary.
	SubmodulePointer    string `yaml:"submodule_pointer"`    // SubmodulePointer marks a submodule whose pointer changed.
	SubmoduleUninit     string `yaml:"submodule_uninit"`     // SubmoduleUninit marks an uninitialized submodule.
	SubmoduleIncomplete string `yaml:"submodule_incomplete"` // SubmoduleIncomplete marks a submodule summary that could not be fully read.
}

type styles struct {
	Clear string // Clear is the style string that clears all styles.

	State  string // State is the style string printed before eventual special state.
	Branch string // Branch is the style string printed before the local branch.
	Remote string // Remote is the style string printed before the upstream branch.

	Divergence string // Divergence is the style string printed before divergence count/symbols.

	Staged    string // Staged is the style string printed before the staged files count.
	Conflict  string // Conflict is the style string printed before the conflict count.
	Modified  string // Modified is the style string printed before the modified files count.
	Untracked string // Untracked is the style string printed before the untracked files count.
	Stashed   string // Stashed is the style string printed before the stash entries count.
	Clean     string // Clean is the style string printed before the clean symbols.

	Insertions string // Insertions is the style string printed before the count of inserted lines.
	Deletions  string // Deletions is the style string printed before the count of deleted lines.

	Submodules string `yaml:"submodules"` // Submodules is the style string printed before the nested submodule summary.
}

const (
	dirLeft   direction = "left"
	dirRight  direction = "right"
	dirCenter direction = "center"
)

type direction string

func (d *direction) UnmarshalYAML(value *yaml.Node) error {
	s := ""
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("error decoding 'direction': %v", s)
	}
	switch direction(s) {
	case dirLeft:
		*d = dirLeft
	case dirRight:
		*d = dirRight
	case dirCenter:
		*d = dirCenter
	default:
		return fmt.Errorf("'direction': unexpected value %v", s)
	}
	return nil
}

type options struct {
	BranchMaxLen      int       `yaml:"branch_max_len"`
	BranchTrim        direction `yaml:"branch_trim"`
	Ellipsis          string    `yaml:"ellipsis"`
	HideClean         bool      `yaml:"hide_clean"`
	DivergenceSpace   bool      `yaml:"divergence_space"`
	SwapDivergence    bool      `yaml:"swap_divergence"`
	FlagsWithoutCount bool      `yaml:"flags_without_count"`

	// SubmoduleMaxDepth bounds the depth of the nested submodule scan (1 =
	// direct submodules only). 0 disables the scan.
	SubmoduleMaxDepth int `yaml:"submodule_max_depth"`
	// SubmoduleTimeout is the total time budget for the nested submodule
	// scan (for example "500ms"). 0 means no extra timeout.
	SubmoduleTimeout Duration `yaml:"submodule_timeout"`
	// SubmoduleMaxShown bounds the number of submodule entries displayed. 0
	// means unlimited.
	SubmoduleMaxShown int `yaml:"submodule_max_shown"`
}

// Duration is a time.Duration that can be decoded from a YAML string such as
// "500ms" or "2s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	du, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(du)
	return nil
}

// Duration returns the underlying time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// A Formater formats git status to a tmux style string.
type Formater struct {
	Config
	st *gitstatus.Status
	// Submodules is the optional nested submodule summary, attached by the
	// caller when the layout includes the 'submodules' component.
	Submodules *submodule.Summary
}

// truncate returns s, truncated so that it is no more than max runes long.
// Depending on the provided direction, truncation is performed right, left or
// center. If s is returned truncated, the truncated part is replaced with the
// 'ellipsis' string.
//
// If max is zero, negative or greater than the number of runes in s, truncate
// just returns s.
//
// NOTE: If max is lower than len(ellipsis), in other words if we're not even
// allowed to just return the ellipsis string, then we just return the maximum
// number of runes we can, without inserting ellipsis.
func truncate(s, ellipsis string, max int, dir direction) string {
	slen := utf8.RuneCountInString(s)
	if max <= 0 || slen <= max {
		return s
	}

	runes := []rune(s)
	ell := []rune(ellipsis)

	if max < len(ellipsis) {
		ell = nil // Just truncate s since even ellipsis don't fit.
	}

	switch dir {
	case dirRight:
		runes = runes[:max-len(ell)]
		runes = append(runes, ell...)
	case dirLeft:
		runes = runes[len(runes)+len(ell)-max:]
		runes = append(ell, runes...)
	case dirCenter:
		// We want to keep the same number of runes on both sides of the ellipsis. If the
		// number of runes on each side is odd, we add one more rune to the right side.
		llen := (max - len(ell)) / 2
		rlen := max - len(ell) - llen

		right := runes[len(runes)-rlen:]

		runes = append(runes[:llen], ell...)
		runes = append(runes, right...)
	}

	return string(runes)
}

// Format writes st as json into w.
func (f *Formater) Format(w io.Writer, st *gitstatus.Status) error {
	defer fmt.Fprintf(w, "%s", f.Styles.Clear)

	f.st = st

	// Overall working tree state
	if f.st.IsInitial {
		branch := truncate(f.st.LocalBranch, f.Options.Ellipsis, f.Options.BranchMaxLen, f.Options.BranchTrim)
		s := fmt.Sprintf("%s%s%s [no commits yet] %s", f.Styles.Clear, f.Styles.Branch, branch, f.flags())
		_, err := io.WriteString(w, s)
		return err
	}

	_, err := fmt.Fprintf(w, "%s%s", f.Styles.Clear, f.format())
	return err
}

const resetStyles = "#[fg=default,bg=default]"

func (f *Formater) format() string {
	var comps []string

	// Add spacing between non-empty components.
	joinComps := func() string {
		i := 0
		for _, s := range comps {
			if s != "" {
				comps[i] = s
				i++
			}
		}
		return strings.Join(comps[:i], " ")
	}

	sb := strings.Builder{}
	for _, item := range f.Layout {
		switch item {
		case "branch":
			comps = append(comps, f.specialState())
		case "remote":
			comps = append(comps, f.remoteBranch())
			comps = append(comps, f.divergence())
		case "remote-branch":
			comps = append(comps, f.remoteBranch())
		case "divergence":
			comps = append(comps, f.divergence())
		case "flags":
			comps = append(comps, f.flags())
		case "stats":
			comps = append(comps, f.stats())
		case "submodules":
			comps = append(comps, f.submodules())
		default:
			sb.WriteString(joinComps())
			sb.WriteString(f.Styles.Clear)
			sb.WriteString(item)
			comps = comps[:0]
		}
	}

	sb.WriteString(joinComps())

	// Reset foreground and background styles to default, since there could be
	// successive elements in user tmux status strings.
	sb.WriteString(resetStyles)
	return sb.String()
}

func (f *Formater) specialState() string {
	s := f.Styles.Clear

	switch f.st.State {
	case gitstatus.Rebasing:
		s += fmt.Sprintf("%s[rebase] ", f.Styles.State)
	case gitstatus.AM:
		s += fmt.Sprintf("%s[am] ", f.Styles.State)
	case gitstatus.AMRebase:
		s += fmt.Sprintf("%s[am-rebase] ", f.Styles.State)
	case gitstatus.Merging:
		s += fmt.Sprintf("%s[merge] ", f.Styles.State)
	case gitstatus.CherryPicking:
		s += fmt.Sprintf("%s[cherry-pick] ", f.Styles.State)
	case gitstatus.Reverting:
		s += fmt.Sprintf("%s[revert] ", f.Styles.State)
	case gitstatus.Bisecting:
		s += fmt.Sprintf("%s[bisect] ", f.Styles.State)
	case gitstatus.Default:
		s += fmt.Sprintf("%s%s", f.Styles.Branch, f.Symbols.Branch)
	}

	s += f.currentRef()
	return s
}

func (f *Formater) remoteBranch() string {
	if f.st.RemoteBranch == "" {
		return ""
	}

	s := f.Styles.Clear

	branch := truncate(f.st.RemoteBranch, f.Options.Ellipsis, f.Options.BranchMaxLen, f.Options.BranchTrim)
	s += fmt.Sprintf("%s%s", f.Styles.Remote, branch)
	return s
}

func (f *Formater) divergence() string {
	if f.st.BehindCount == 0 && f.st.AheadCount == 0 {
		return ""
	}

	behind := ""
	ahead := ""
	s := f.Styles.Clear + f.Styles.Divergence
	if f.st.BehindCount != 0 {
		behind = fmt.Sprintf("%s%d", f.Symbols.Behind, f.st.BehindCount)
	}

	if f.st.AheadCount != 0 {
		ahead = fmt.Sprintf("%s%d", f.Symbols.Ahead, f.st.AheadCount)
	}

	// Handle 'swap divergence'
	var left, right string
	if !f.Options.SwapDivergence {
		left, right = behind, ahead
	} else {
		left, right = ahead, behind
	}

	// Handle 'divergence space'
	space := ""
	if f.Options.DivergenceSpace && right != "" && left != "" {
		space = " "
	}
	return s + left + space + right
}

func (f *Formater) currentRef() string {
	if f.st.IsDetached {
		return fmt.Sprintf("%s%s%s%s", f.Styles.Clear, f.Styles.Branch, f.Symbols.HashPrefix, f.st.HEAD)
	}

	branch := truncate(f.st.LocalBranch, f.Options.Ellipsis, f.Options.BranchMaxLen, f.Options.BranchTrim)
	return fmt.Sprintf("%s%s%s", f.Styles.Clear, f.Styles.Branch, branch)
}

// appendFlag appends a flag to the flags slice based on configuration options
func (f *Formater) appendFlag(flags []string, style, symbol string, count int) []string {
	if count == 0 {
		return flags
	}

	if f.Options.FlagsWithoutCount {
		// When flags_without_count is true, show symbol only (empty string if symbol is empty)
		if symbol == "" {
			return flags
		}
		return append(flags, fmt.Sprintf("%s%s", style, symbol))
	}

	// When flags_without_count is false, show symbol + count, or just count if symbol is empty
	return append(flags, fmt.Sprintf("%s%s%d", style, symbol, count))
}

func (f *Formater) flags() string {
	var flags []string
	if f.st.IsClean {
		// For stashed in clean state, handle empty symbols properly
		flags = f.appendFlag(flags, f.Styles.Stashed, f.Symbols.Stashed, f.st.NumStashed)

		// Clean flag only shows if symbol is not empty and hide_clean is false
		if !f.Options.HideClean && f.Symbols.Clean != "" {
			flags = append(flags, fmt.Sprintf("%s%s", f.Styles.Clean, f.Symbols.Clean))
		}

		if len(flags) != 0 {
			return f.Styles.Clear + strings.Join(flags, " ")
		}
	}

	// For all other flags, handle empty symbols properly
	flags = f.appendFlag(flags, f.Styles.Staged, f.Symbols.Staged, f.st.NumStaged)
	flags = f.appendFlag(flags, f.Styles.Conflict, f.Symbols.Conflict, f.st.NumConflicts)
	flags = f.appendFlag(flags, f.Styles.Modified, f.Symbols.Modified, f.st.NumModified)
	flags = f.appendFlag(flags, f.Styles.Stashed, f.Symbols.Stashed, f.st.NumStashed)
	flags = f.appendFlag(flags, f.Styles.Untracked, f.Symbols.Untracked, f.st.NumUntracked)

	if len(flags) > 0 {
		return f.Styles.Clear + strings.Join(flags, " ")
	}

	return ""
}

// submoduleNotable reports whether a submodule state carries something to
// display. Fully initialized, clean submodules are hidden.
func submoduleNotable(st *submodule.State) bool {
	return st.Uninitialized || st.PointerChanged || st.Conflict ||
		st.NumStaged > 0 || st.NumConflicts > 0 || st.NumModified > 0 ||
		st.NumUntracked > 0 || st.Incomplete
}

// submoduleEntry renders a single submodule state: its path followed by the
// pointer marker, internal change flags (staged/conflict/modified/untracked),
// the uninitialized marker and the incomplete marker.
func (f *Formater) submoduleEntry(st *submodule.State) string {
	sb := strings.Builder{}
	sb.WriteString(f.Styles.Clear)
	sb.WriteString(f.Styles.Submodules)
	sb.WriteString(st.Path)

	marker := func(sym string) {
		if sym == "" {
			return
		}
		sb.WriteString(" ")
		sb.WriteString(f.Styles.Clear)
		sb.WriteString(f.Styles.Submodules)
		sb.WriteString(sym)
	}

	if st.PointerChanged {
		marker(f.Symbols.SubmodulePointer)
	}

	// A conflicted gitlink in the superproject counts as one conflict, in
	// addition to conflicts internal to the submodule.
	nConflict := st.NumConflicts
	if st.Conflict {
		nConflict++
	}

	var flags []string
	flags = f.appendFlag(flags, f.Styles.Staged, f.Symbols.Staged, st.NumStaged)
	flags = f.appendFlag(flags, f.Styles.Conflict, f.Symbols.Conflict, nConflict)
	flags = f.appendFlag(flags, f.Styles.Modified, f.Symbols.Modified, st.NumModified)
	flags = f.appendFlag(flags, f.Styles.Untracked, f.Symbols.Untracked, st.NumUntracked)
	if len(flags) > 0 {
		sb.WriteString(" ")
		sb.WriteString(strings.Join(flags, " "))
	}

	if st.Uninitialized {
		marker(f.Symbols.SubmoduleUninit)
	}
	if st.Incomplete {
		marker(f.Symbols.SubmoduleIncomplete)
	}
	return sb.String()
}

// submodules renders the optional nested submodule summary. Parent repository
// counts are never modified by this summary: submodule states are reported in
// this dedicated component only.
func (f *Formater) submodules() string {
	sum := f.Submodules
	if sum == nil {
		return ""
	}

	var entries []string
	hidden := 0
	for _, st := range sum.States {
		if !submoduleNotable(st) {
			continue
		}
		if max := f.Options.SubmoduleMaxShown; max > 0 && len(entries) >= max {
			hidden++
			continue
		}
		entries = append(entries, f.submoduleEntry(st))
	}

	if len(entries) == 0 && hidden == 0 && !sum.Incomplete {
		return ""
	}

	// Segments joined with ", ". The optional prefix symbol is not a
	// segment (it may itself end with a space).
	segs := entries
	if hidden > 0 {
		segs = append(segs, fmt.Sprintf("+%d", hidden))
	}
	if sum.Incomplete {
		// The traversal timed out or failed to read some submodule.
		if f.Symbols.SubmoduleIncomplete != "" {
			segs = append(segs, f.Symbols.SubmoduleIncomplete)
		} else {
			segs = append(segs, "?")
		}
	}

	sb := strings.Builder{}
	sb.WriteString(f.Styles.Clear)
	sb.WriteString(f.Styles.Submodules)
	if f.Symbols.Submodules != "" {
		sb.WriteString(f.Symbols.Submodules)
	}
	for i, seg := range segs {
		if i > 0 {
			sb.WriteString(f.Styles.Clear)
			sb.WriteString(f.Styles.Submodules)
			sb.WriteString(", ")
		}
		sb.WriteString(seg)
	}
	return sb.String()
}

func (f *Formater) stats() string {
	stats := make([]string, 0, 2)

	if f.st.Insertions != 0 {
		stats = append(stats, fmt.Sprintf("%s%s%d", f.Styles.Insertions, f.Symbols.Insertions, f.st.Insertions))
	}

	if f.st.Deletions != 0 {
		stats = append(stats, fmt.Sprintf("%s%s%d", f.Styles.Deletions, f.Symbols.Deletions, f.st.Deletions))
	}

	if len(stats) == 0 {
		return ""
	}

	return f.Styles.Clear + strings.Join(stats, " ")
}
