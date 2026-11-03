// Package navstat reads how an agent found its way around a repository from Claude Code's session transcript (the
// same JSON lines contextstat reads): how much it searched before its first edit, what it read again and again, what
// it searched for in vain, which docs it read before editing elsewhere, and the whole files it read. Digest folds the
// sessions of a repository's tasks into a short text for the model of the navigation retro.
package navstat

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// BigRead: a whole file read (no offset or limit) longer than this, in characters (≈ 10k tokens), is a big read.
const BigRead = 40_000

// Session is what one transcript came to.
type Session struct {
	// Searches: the searching calls (Grep, Glob, Read, Bash with grep/find/ls/cat…) before the first edit, all of
	// them when the agent never edited.
	Searches int
	// Queries: the first of those searches, as «Grep: pattern».
	Queries []string
	Edited  bool
	// Reads: each file read, how many times and how much came back.
	Reads map[string]*Read
	// Empty: the searches that found nothing.
	Empty []string
	// DocsBeforeEdit: the docs (*.md, docs/) read before the first edit.
	DocsBeforeEdit []string
	// EditedFiles: the files the agent edited or wrote.
	EditedFiles []string
	// Big: the whole files read longer than BigRead.
	Big []Read
}

// Read is a file and how it was read.
type Read struct {
	File  string `json:"file"`
	Count int    `json:"count"`
	Chars int    `json:"chars"`
}

// Tokens: the characters as tokens, roughly.
func (r Read) Tokens() int { return r.Chars / 4 }

// maxQueries: how many of the searches before the first edit a session keeps as examples.
const maxQueries = 8

type call struct {
	name, file, query string
	search, whole     bool
	edit              bool
	// files: what a shell command (cat, sed -n, head, tail) read.
	files []string
}

type analyzer struct {
	root string
	// cwd: the shell's directory, relative to root ("" the root itself): Claude Code keeps a «cd» for the next calls.
	cwd   string
	calls map[string]call
	s     Session
}

// AnalyzeFile reads a transcript; root is the worktree the session ran in, the files are named relative to it.
// An unreadable file is an empty session and false.
func AnalyzeFile(path, root string) (Session, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Session{Reads: map[string]*Read{}}, false
	}
	defer f.Close()
	return Analyze(f, root), true
}

// Analyze reads the JSON lines of a session; root is the worktree it ran in.
func Analyze(r io.Reader, root string) Session {
	a := &analyzer{root: strings.TrimRight(root, "/"), calls: map[string]call{}, s: Session{Reads: map[string]*Read{}}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		a.line(sc.Bytes())
	}
	return a.s
}

func (a *analyzer) line(line []byte) {
	var e struct {
		Type      string          `json:"type"`
		Parent    *string         `json:"parent_tool_use_id"`
		Sidechain bool            `json:"isSidechain"`
		Message   json.RawMessage `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.Parent != nil || e.Sidechain {
		return
	}
	switch e.Type {
	case "assistant":
		a.assistant(e.Message)
	case "user":
		a.user(e.Message)
	}
}

func (a *analyzer) assistant(raw json.RawMessage) {
	var m struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	for _, c := range m.Content {
		if c.Type != "tool_use" || c.ID == "" {
			continue
		}
		if _, ok := a.calls[c.ID]; ok {
			continue // the same call, on another line of its message
		}
		x := a.callOf(c.Name, c.Input)
		a.calls[c.ID] = x
		switch {
		case x.edit:
			a.s.Edited = true
			if x.file != "" && !slices.Contains(a.s.EditedFiles, x.file) {
				a.s.EditedFiles = append(a.s.EditedFiles, x.file)
			}
		case x.search && !a.s.Edited:
			a.s.Searches++
			if len(a.s.Queries) < maxQueries {
				a.s.Queries = append(a.s.Queries, x.query)
			}
			for _, f := range append([]string{x.file}, x.files...) {
				if f != "" && isDoc(f) && !slices.Contains(a.s.DocsBeforeEdit, f) {
					a.s.DocsBeforeEdit = append(a.s.DocsBeforeEdit, f)
				}
			}
		}
	}
}

func (a *analyzer) user(raw json.RawMessage) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		x, ok := a.calls[b.ToolUseID]
		if !ok || b.IsError {
			continue
		}
		text := resultText(b.Content)
		switch x.name {
		case "Read":
			if x.file != "" {
				a.read(x.file, utf8.RuneCountInString(text), x.whole)
			}
		case "Grep", "Glob":
			if empty(text) {
				a.s.Empty = append(a.s.Empty, x.query)
			}
		case "Bash":
			// What came back is shared between the files the command read: a guess, but the counts are exact.
			n := utf8.RuneCountInString(text)
			for _, f := range x.files {
				a.read(f, n/len(x.files), x.whole && len(x.files) == 1)
			}
			if x.search && strings.TrimSpace(text) == "" && shellSearch.MatchString(firstCommand(x.query[len("Bash: "):])) {
				a.s.Empty = append(a.s.Empty, x.query)
			}
		}
	}
}

// read counts a file read; whole: all of it, not a part.
func (a *analyzer) read(file string, chars int, whole bool) {
	r := a.s.Reads[file]
	if r == nil {
		r = &Read{File: file}
		a.s.Reads[file] = r
	}
	r.Count++
	r.Chars += chars
	if whole && chars > BigRead {
		a.s.Big = append(a.s.Big, Read{File: file, Count: 1, Chars: chars})
	}
}

// shellSearch: a shell command whose empty output means it found nothing.
var shellSearch = regexp.MustCompile(`^(grep|rg|ag|git grep|find|fd)\b`)

// searchCommand: a shell command that only looks around the repository.
var searchCommand = regexp.MustCompile(`^(grep|rg|ag|find|fd|ls|tree|cat|head|tail|less|wc|sed -n|git grep|git ls-files)\b`)

func (a *analyzer) callOf(name string, input json.RawMessage) call {
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Pattern      string `json:"pattern"`
		Path         string `json:"path"`
		Glob         string `json:"glob"`
		Command      string `json:"command"`
		Offset       *int   `json:"offset"`
		Limit        *int   `json:"limit"`
	}
	json.Unmarshal(input, &in)
	x := call{name: name}
	switch name {
	case "Read":
		file, inside := a.inside(in.FilePath)
		if !inside {
			return x // a screenshot, a temp file: not the repository
		}
		x.file = file
		x.search, x.whole = true, in.Offset == nil && in.Limit == nil
		x.query = "Read: " + x.file
	case "Grep", "Glob":
		x.search = true
		x.query = name + ": " + clip(in.Pattern, 80)
		if where := a.rel(in.Path); where != "" && where != "." {
			x.query += " in " + where
		} else if in.Glob != "" {
			x.query += " in " + in.Glob
		}
	case "Bash":
		a.chdir(in.Command)
		if file, ok := a.bashEdit(in.Command); ok {
			x.edit, x.file = true, file
			return x
		}
		if cmd := firstCommand(in.Command); searchCommand.MatchString(cmd) {
			x.search, x.query = true, "Bash: "+clip(cmd, 100)
			x.files, x.whole = a.shellReads(in.Command)
		}
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		// A plan or a note outside the worktree is not a change to the repository.
		if file, inside := a.inside(in.FilePath + in.NotebookPath); inside {
			x.edit, x.file = true, file
		}
	}
	return x
}

// Shell commands that change files: in place (sed -i, perl -i), a script that writes, a patch, or output sent to a
// file (cat > f <<EOF, tee f).
var (
	inPlace     = regexp.MustCompile(`\b(sed|perl)\s+(-[a-zA-Z]*i|--in-place)|\bgit apply\b|\bpatch\s+-p`)
	script      = regexp.MustCompile(`^(python3?|node|ruby|perl)\b`)
	writes      = regexp.MustCompile(`\.write\(|write_text\(|writeFileSync\(|File\.write\(`)
	quoted      = regexp.MustCompile(`'[^']*'|"(?:[^"\\]|\\.)*"`)
	redirection = regexp.MustCompile(`(?:^|[^0-9&><=-])(?:>>?|\btee\s+(?:-a\s+)?)\s*([^\s&|;<>()'"]+)`)
)

// bashEdit: the command changes a file of the worktree, and which one when it says so plainly.
func (a *analyzer) bashEdit(command string) (string, bool) {
	first := firstCommand(command)
	if first == "" {
		return "", false
	}
	// The redirections of the first line, quotes left out: a heredoc's body or a quoted «=>» is not one.
	bare := quoted.ReplaceAllString(first, `""`)
	for _, m := range redirection.FindAllStringSubmatch(bare, -1) {
		if file, ok := a.inside(m[1]); ok && !strings.HasPrefix(m[1], "/dev/") {
			return file, true
		}
	}
	if inPlace.MatchString(bare) {
		return "", true
	}
	return "", script.MatchString(first) && writes.MatchString(command)
}

// shellSeparators: what splits a shell line into commands.
var shellSeparators = regexp.MustCompile(`&&|\|\||[;|\n]`)

// shellReads: the files of the worktree a shell command printed (cat, sed -n, head, tail), and whether it printed
// them whole (cat).
func (a *analyzer) shellReads(command string) (files []string, whole bool) {
	whole = true
	for _, part := range shellSeparators.Split(command, -1) {
		args := strings.Fields(quoted.ReplaceAllStringFunc(part, func(q string) string { return strings.ReplaceAll(q, " ", "\x00") }))
		if len(args) > 0 && args[0] == "rtk" {
			args = args[1:]
		}
		if len(args) < 2 {
			continue
		}
		script := false
		switch args[0] {
		case "cat":
		case "head", "tail", "less":
			whole = false
		case "sed":
			if !slices.Contains(args, "-n") {
				continue
			}
			whole, script = false, true
		default:
			continue
		}
		for _, arg := range args[1:] {
			if strings.HasPrefix(arg, "-") || strings.ContainsAny(arg, "*?<>$`") {
				continue
			}
			if script { // sed's own script comes first
				script = false
				continue
			}
			arg = strings.Trim(strings.ReplaceAll(arg, "\x00", " "), `"'`)
			if !strings.Contains(arg, ".") && !strings.Contains(arg, "/") {
				continue // a number of lines, a word
			}
			if f, ok := a.inside(arg); ok && !slices.Contains(files, f) {
				files = append(files, f)
			}
		}
	}
	if strings.Contains(command, "<<") {
		return nil, false // a heredoc: what it «cats» is being written
	}
	return files, whole && len(files) > 0
}

// firstCommand: the first line of a shell command, without the «cd dir &&» before it or the «rtk» proxy.
func firstCommand(command string) string {
	cmd, _, _ := strings.Cut(strings.TrimSpace(command), "\n")
	cmd = strings.TrimSpace(strings.TrimPrefix(cmd, "rtk "))
	if c, rest, ok := strings.Cut(cmd, "&&"); ok && strings.HasPrefix(strings.TrimSpace(c), "cd ") {
		cmd = strings.TrimSpace(rest)
	}
	return strings.TrimSpace(strings.TrimPrefix(cmd, "rtk "))
}

// chdir follows a «cd dir» at the start of a shell command: the paths of this command and the next ones are
// relative to it. A directory outside the worktree leaves the shell's one unknown, taken as the root.
func (a *analyzer) chdir(command string) {
	cmd, _, _ := strings.Cut(strings.TrimSpace(command), "\n")
	first := strings.TrimSpace(shellSeparators.Split(cmd, 2)[0])
	f := strings.Fields(first)
	if len(f) != 2 || f[0] != "cd" {
		return
	}
	dir := strings.Trim(f[1], `"'`)
	if dir, ok := a.inside(dir); ok {
		a.cwd = dir
		if a.cwd == "." {
			a.cwd = ""
		}
	} else {
		a.cwd = ""
	}
}

// inside: a path relative to the worktree, and whether it is in it; a relative path is taken from the shell's
// directory.
func (a *analyzer) inside(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Clean(filepath.Join(a.cwd, p))
		return p, !strings.HasPrefix(p, "..")
	}
	if a.root == "" {
		return p, true
	}
	r, err := filepath.Rel(a.root, p)
	if err != nil || strings.HasPrefix(r, "..") {
		return p, false
	}
	return r, true
}

// rel: a path relative to the worktree; one outside it stays as it is.
func (a *analyzer) rel(p string) string {
	if p == "" || a.root == "" || !filepath.IsAbs(p) {
		return p
	}
	if r, err := filepath.Rel(a.root, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

func isDoc(file string) bool {
	f := strings.ToLower(file)
	return strings.HasSuffix(f, ".md") || strings.HasPrefix(f, "docs/") || strings.Contains(f, "/docs/")
}

// empty: a Grep's or Glob's result that found nothing.
func empty(text string) bool {
	t := strings.TrimSpace(text)
	return t == "" || strings.HasPrefix(t, "No matches found") || strings.HasPrefix(t, "No files found") || t == "Found 0 files"
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &blocks)
	var b strings.Builder
	for _, x := range blocks {
		if x.Type == "text" {
			b.WriteString(x.Text)
		}
	}
	return b.String()
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// Task is the session of one task, for Digest.
type Task struct {
	ID      string
	Title   string
	Session Session
}

// maxDigest bounds the digest, in characters.
const maxDigest = 12_000

// Digest folds the tasks' sessions into a short text: the repository's figures, the files read in many tasks, the
// searches that found nothing, the docs read before an edit elsewhere, the big reads, and the tasks that searched
// the longest. The model of the navigation retro gets this, not the transcripts.
func Digest(tasks []Task) string {
	if len(tasks) == 0 {
		return ""
	}
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	n := len(tasks)

	searches := make([]int, 0, n)
	edited := 0
	for _, t := range tasks {
		searches = append(searches, t.Session.Searches)
		if t.Session.Edited {
			edited++
		}
	}
	total := 0
	for _, s := range searches {
		total += s
	}
	sorted := slices.Clone(searches)
	sort.Ints(sorted)
	line("Tasks analyzed: %d (the agent edited files in %d).", n, edited)
	line("Search calls (Grep, Glob, Read, shell grep/find/ls/cat) before the first edit: average %.1f, median %d, most %d.",
		float64(total)/float64(n), sorted[n/2], sorted[n-1])

	// Files read across tasks.
	type across struct {
		file          string
		tasks, reads  int
		chars, maxPer int
	}
	files := map[string]*across{}
	for _, t := range tasks {
		for f, r := range t.Session.Reads {
			x := files[f]
			if x == nil {
				x = &across{file: f}
				files[f] = x
			}
			x.tasks++
			x.reads += r.Count
			x.chars += r.Chars
			x.maxPer = max(x.maxPer, r.Count)
		}
	}
	list := make([]*across, 0, len(files))
	for _, x := range files {
		list = append(list, x)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].tasks != list[j].tasks {
			return list[i].tasks > list[j].tasks
		}
		if list[i].chars != list[j].chars {
			return list[i].chars > list[j].chars
		}
		return list[i].file < list[j].file
	})
	if len(list) > 0 {
		line("\nFiles read in the most tasks (tasks of %d, reads in all, ≈ tokens in all, most reads in one task):", n)
		for _, x := range list[:min(10, len(list))] {
			line("- %s: %d of %d tasks, %d reads, ≈%d tokens, up to %d in one task", x.file, x.tasks, n, x.reads, x.chars/4, x.maxPer)
		}
	}

	// Read again within one session.
	type again struct {
		task, file string
		count      int
	}
	var agains []again
	for _, t := range tasks {
		for f, r := range t.Session.Reads {
			if r.Count > 2 {
				agains = append(agains, again{t.ID, f, r.Count})
			}
		}
	}
	sort.Slice(agains, func(i, j int) bool {
		if agains[i].count != agains[j].count {
			return agains[i].count > agains[j].count
		}
		return agains[i].task+agains[i].file < agains[j].task+agains[j].file
	})
	if len(agains) > 0 {
		line("\nThe same file read three times or more in one task:")
		for _, x := range agains[:min(8, len(agains))] {
			line("- %s: %s, %d times", x.task, x.file, x.count)
		}
	}

	// Searches that found nothing.
	emptyCount := map[string]int{}
	emptyTotal := 0
	for _, t := range tasks {
		emptyTotal += len(t.Session.Empty)
		for _, q := range t.Session.Empty {
			emptyCount[q]++
		}
	}
	if emptyTotal > 0 {
		qs := make([]string, 0, len(emptyCount))
		for q := range emptyCount {
			qs = append(qs, q)
		}
		sort.Slice(qs, func(i, j int) bool {
			if emptyCount[qs[i]] != emptyCount[qs[j]] {
				return emptyCount[qs[i]] > emptyCount[qs[j]]
			}
			return qs[i] < qs[j]
		})
		line("\nSearches that found nothing: %d in all. Some of them:", emptyTotal)
		for _, q := range qs[:min(10, len(qs))] {
			line("- %s (%d)", q, emptyCount[q])
		}
	}

	// Docs read before the first edit, when the edit went elsewhere.
	var docs []string
	for _, t := range tasks {
		s := t.Session
		if !s.Edited || len(s.DocsBeforeEdit) == 0 {
			continue
		}
		for _, d := range s.DocsBeforeEdit {
			if !slices.Contains(s.EditedFiles, d) {
				docs = append(docs, fmt.Sprintf("- %s: read %s, then edited %s", t.ID, d, strings.Join(first(s.EditedFiles, 4), ", ")))
			}
		}
	}
	if len(docs) > 0 {
		line("\nDocs read before the first edit that went to other files:")
		for _, d := range docs[:min(10, len(docs))] {
			line("%s", d)
		}
	}

	// Whole files read that are big.
	big := map[string]Read{}
	for _, t := range tasks {
		for _, r := range t.Session.Big {
			x := big[r.File]
			x.File, x.Count, x.Chars = r.File, x.Count+1, max(x.Chars, r.Chars)
			big[r.File] = x
		}
	}
	if len(big) > 0 {
		bs := make([]Read, 0, len(big))
		for _, r := range big {
			bs = append(bs, r)
		}
		sort.Slice(bs, func(i, j int) bool {
			if bs[i].Count != bs[j].Count {
				return bs[i].Count > bs[j].Count
			}
			return bs[i].Chars > bs[j].Chars
		})
		line("\nWhole files read that are big (over ≈%d tokens):", BigRead/4)
		for _, r := range bs[:min(8, len(bs))] {
			line("- %s: ≈%d tokens, read whole %d times", r.File, r.Tokens(), r.Count)
		}
	}

	// The tasks that searched the longest.
	byLong := slices.Clone(tasks)
	sort.SliceStable(byLong, func(i, j int) bool { return byLong[i].Session.Searches > byLong[j].Session.Searches })
	line("\nThe tasks that searched the longest before their first edit:")
	for _, t := range byLong[:min(5, len(byLong))] {
		title := ""
		if t.Title != "" {
			title = " (" + clip(t.Title, 80) + ")"
		}
		line("- %s%s: %d searches; first ones: %s", t.ID, title, t.Session.Searches, strings.Join(t.Session.Queries, "; "))
	}

	out := b.String()
	if r := []rune(out); len(r) > maxDigest {
		out = string(r[:maxDigest]) + "\n…"
	}
	return out
}

func first(s []string, n int) []string {
	if len(s) > n {
		return append(slices.Clone(s[:n]), "…")
	}
	return s
}
