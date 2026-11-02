package chat

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Turn: one message of yours, or everything the agent did between two of them: what it said and the steps it
// took, drawn under one name.
type Turn struct {
	Who    string   `json:"who"` // "user", "agent"
	At     int64    `json:"at,omitempty"`
	Item   int      `json:"item"`             // a message of yours: its item
	Prompt bool     `json:"prompt,omitempty"` // the task Mako handed the agent, folded
	Text   string   `json:"text,omitempty"`   // a message of yours without its pictures
	Images []string `json:"images,omitempty"` // the pictures attached to it: their files
	Blocks []Block  `json:"blocks,omitempty"`
}

// Block: one thing in the agent's turn. "item" is one item drawn as it is (what it said, a question, the plan, a
// screenshot, an error, the end of the turn); the others gather steps: "commands", "reads", "edits", "search",
// "web", "steps" (a run of several kinds), "current" (the step it runs now).
type Block struct {
	Kind    string  `json:"kind"`
	Item    int     `json:"item"`
	Rows    []Row   `json:"rows,omitempty"`
	Failed  int     `json:"failed,omitempty"` // steps that failed
	Running bool    `json:"running,omitempty"`
	Seconds float64 `json:"seconds,omitempty"` // from its first step's start to its last step's end
}

// Row: one step of a block, said as a person would: the command without «cd <worktree> &&», the file relative to
// the worktree; a test run with how many passed and failed, a commit with its hash.
type Row struct {
	I      int    `json:"i"`
	Label  string `json:"label"`
	Test   bool   `json:"test,omitempty"`
	Passed int    `json:"passed,omitempty"`
	Failed int    `json:"failed,omitempty"`
	Commit string `json:"commit,omitempty"`
	Add    int    `json:"add,omitempty"`
	Del    int    `json:"del,omitempty"`
}

// Progress: the agent's own plan (its last TodoWrite).
type Progress struct {
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Current string `json:"current,omitempty"`
	Steps   []Todo `json:"steps"`
}

type Todo struct {
	Text   string `json:"text"`
	Status string `json:"status"` // "pending", "in_progress", "completed"
}

// TurnStat: the turn now going on: when it began, how many steps it took, and what it cost so far (USD,
// Estimate while only the tokens tell it).
type TurnStat struct {
	Since    int64   `json:"since,omitempty"`
	Steps    int     `json:"steps"`
	Cost     float64 `json:"cost,omitempty"`
	Estimate bool    `json:"estimate,omitempty"`
}

// PromptAt: a first message of yours this long is the task Mako handed the agent.
const PromptAt = 400

func category(it Item) string {
	switch it.Tool {
	case "Bash", "BashOutput", "KillShell", "Setup":
		return "commands"
	case "Read":
		return "reads"
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return "edits"
	case "Grep", "Glob", "LS":
		return "search"
	case "WebFetch", "WebSearch":
		return "web"
	}
	return ""
}

// Group puts the conversation into turns and the agent's steps into blocks.
func Group(items []Item, worktree string) []Turn {
	var turns []Turn
	var cur *Turn
	var run []int // the quiet steps in a row not drawn yet
	flush := func() {
		if len(run) > 0 && cur != nil {
			cur.Blocks = append(cur.Blocks, blocksOf(items, run, worktree)...)
		}
		run = nil
	}
	for i, it := range items {
		if it.Kind == "user" {
			flush()
			text, images := splitImages(it.Text)
			turns = append(turns, Turn{Who: "user", At: it.At, Item: i, Text: text, Images: images,
				Prompt: i == 0 && len(it.Text) > PromptAt})
			cur = nil
			continue
		}
		if cur == nil {
			turns = append(turns, Turn{Who: "agent", At: it.At})
			cur = &turns[len(turns)-1]
		}
		switch {
		case it.Kind == "tool" && it.Tool == "TodoWrite":
			// Shown as the progress above the conversation.
		case it.Kind == "tool" && it.Status == "running":
			flush()
			cur.Blocks = append(cur.Blocks, Block{Kind: "current", Item: i, Running: true, Rows: []Row{rowOf(items, i, worktree)}})
		case it.Kind == "tool" && category(it) != "":
			run = append(run, i)
		default:
			flush()
			cur.Blocks = append(cur.Blocks, Block{Kind: "item", Item: i, Failed: b2i(it.Kind == "tool" && it.Status == "error")})
		}
	}
	flush()
	return turns
}

// blocksOf: a run of steps with nothing said between them. Steps of one kind in a row make one block; a long run
// that switches kinds often (three blocks or more) folds into one «steps» block, unless it edits: an edit always
// shows.
func blocksOf(items []Item, run []int, worktree string) []Block {
	segments, edits := 0, false
	for n, i := range run {
		k := category(items[i])
		if n == 0 || k != category(items[run[n-1]]) {
			segments++
		}
		edits = edits || k == "edits"
	}
	if len(run) >= 4 && segments >= 3 && !edits {
		return []Block{block("steps", items, run, worktree)}
	}
	var out []Block
	for a := 0; a < len(run); {
		k := category(items[run[a]])
		b := a + 1
		for b < len(run) && category(items[run[b]]) == k {
			b++
		}
		out = append(out, block(k, items, run[a:b], worktree))
		a = b
	}
	return out
}

func block(kind string, items []Item, idx []int, worktree string) Block {
	b := Block{Kind: kind, Item: idx[0]}
	var first, last int64
	for _, i := range idx {
		it := items[i]
		b.Rows = append(b.Rows, rowOf(items, i, worktree))
		if it.Status == "error" {
			b.Failed++
		}
		if it.Status == "running" {
			b.Running = true
		}
		if it.At > 0 && (first == 0 || it.At < first) {
			first = it.At
		}
		if it.End > last {
			last = it.End
		}
	}
	if first > 0 && last > first {
		b.Seconds = float64(last-first) / 1000
	}
	return b
}

func rowOf(items []Item, i int, worktree string) Row {
	it := items[i]
	var in map[string]any
	json.Unmarshal(it.Input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	r := Row{I: i}
	switch category(it) {
	case "commands":
		cmd := StripCd(str("command"), worktree)
		r.Label = firstLine(cmd)
		if r.Label == "" {
			r.Label = str("description")
		}
		if it.Status != "running" {
			r.Passed, r.Failed, r.Test = TestCounts(cmd, it.Output)
			r.Commit = CommitHash(cmd, it.Output)
		}
	case "reads", "edits":
		p := str("file_path")
		if p == "" {
			p = str("notebook_path")
		}
		r.Label = Rel(p, worktree)
		if category(it) == "edits" {
			r.Add, r.Del = editCount(it.Tool, in)
		}
	case "search":
		r.Label = str("pattern")
		if p := str("path"); p != "" {
			r.Label += " in " + Rel(p, worktree)
		}
	case "web":
		r.Label = str("url")
		if r.Label == "" {
			r.Label = str("query")
		}
	default:
		r.Label = strings.TrimPrefix(toolArg(it.Input), ": ")
	}
	return r
}

func editCount(tool string, in map[string]any) (add, del int) {
	n := func(v any) int {
		s, _ := v.(string)
		if s == "" {
			return 0
		}
		return strings.Count(s, "\n") + 1
	}
	switch tool {
	case "Write":
		return n(in["content"]), 0
	case "NotebookEdit":
		return n(in["new_source"]), 0
	case "Edit":
		return n(in["new_string"]), n(in["old_string"])
	case "MultiEdit":
		edits, _ := in["edits"].([]any)
		for _, e := range edits {
			m, _ := e.(map[string]any)
			add += n(m["new_string"])
			del += n(m["old_string"])
		}
	}
	return add, del
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Rel: path relative to the worktree when it is inside it.
func Rel(path, worktree string) string {
	if worktree != "" {
		if r, err := filepath.Rel(worktree, path); err == nil && r != "." && !strings.HasPrefix(r, "..") && filepath.IsAbs(path) {
			return r
		}
	}
	return path
}

var cdPrefix = regexp.MustCompile(`^\s*cd\s+("[^"]*"|'[^']*'|[^\s;&|]+)\s*&&\s*`)

// StripCd drops the «cd <dir> && » a command begins with when dir is the worktree or inside it (any dir when the
// worktree is not known).
func StripCd(cmd, worktree string) string {
	for {
		m := cdPrefix.FindStringSubmatchIndex(cmd)
		if m == nil {
			return strings.TrimSpace(cmd)
		}
		dir := strings.Trim(cmd[m[2]:m[3]], `"'`)
		if worktree != "" && dir != worktree && !strings.HasPrefix(dir, strings.TrimSuffix(worktree, "/")+"/") && filepath.IsAbs(dir) {
			return strings.TrimSpace(cmd)
		}
		cmd = cmd[m[1]:]
	}
}

var (
	ansi       = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	testCmd    = regexp.MustCompile(`(?:^|[\s;&|(])(?:go\s+test|gotestsum|(?:npm|pnpm|yarn|bun)\s+(?:run\s+)?test\S*|npx\s+(?:vitest|jest)|vitest|jest|pytest|python3?\s+-m\s+pytest)(?:\s|$)`)
	goPass     = regexp.MustCompile(`(?m)^\s*--- PASS:`)
	goFail     = regexp.MustCompile(`(?m)^\s*--- FAIL:`)
	goPkgOK    = regexp.MustCompile(`(?m)^ok\s+\S+`)
	goPkgFail  = regexp.MustCompile(`(?m)^FAIL\s+\S+\s+[\d.]+s`)
	goDone     = regexp.MustCompile(`DONE (\d+) tests?(?:, (\d+) failures?)?`)
	jsTests    = regexp.MustCompile(`(?m)^\s*Tests:?\s+(.*)$`)
	pyResult   = regexp.MustCompile(`(?m)^=+ (.*\b(?:passed|failed)\b.*) in [\d.]+s.*=+\s*$`)
	countOf    = func(word string) *regexp.Regexp { return regexp.MustCompile(`(\d+) ` + word) }
	passedN    = countOf("passed")
	failedN    = countOf("failed")
	errorsN    = regexp.MustCompile(`(\d+) errors?\b`)
	commitLine = regexp.MustCompile(`(?m)^\[[^\]\s]+(?: \([^)]*\))? ([0-9a-f]{7,40})\]`)
	gitCommit  = regexp.MustCompile(`(?:^|[\s;&|])git\s+(?:-\S+\s+(?:\S+\s+)?)*commit\b`)
)

func num(re *regexp.Regexp, s string) int {
	if m := re.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// TestCounts: how many tests passed and failed in a test run's output; ok is false when the command runs no tests
// or the output does not say.
func TestCounts(cmd, output string) (passed, failed int, ok bool) {
	if !testCmd.MatchString(cmd) {
		return 0, 0, false
	}
	out := ansi.ReplaceAllString(output, "")
	if m := goDone.FindStringSubmatch(out); m != nil { // gotestsum
		total, _ := strconv.Atoi(m[1])
		f, _ := strconv.Atoi(m[2])
		return total - f, f, true
	}
	if m := pyResult.FindAllStringSubmatch(out, -1); m != nil {
		line := m[len(m)-1][1]
		return num(passedN, line), num(failedN, line) + num(errorsN, line), true
	}
	if m := jsTests.FindAllStringSubmatch(out, -1); m != nil {
		for k := len(m) - 1; k >= 0; k-- {
			if line := m[k][1]; passedN.MatchString(line) || failedN.MatchString(line) {
				return num(passedN, line), num(failedN, line), true
			}
		}
	}
	p, f := len(goPass.FindAllString(out, -1)), len(goFail.FindAllString(out, -1))
	if p+f > 0 {
		return p, f, true
	}
	// go test without -v says only how its packages went.
	p, f = len(goPkgOK.FindAllString(out, -1)), len(goPkgFail.FindAllString(out, -1))
	if p+f > 0 {
		return p, f, true
	}
	return 0, 0, false
}

// CommitHash: the hash git commit printed («[main a91c0e2] message»), "" for anything else.
func CommitHash(cmd, output string) string {
	if !gitCommit.MatchString(cmd) {
		return ""
	}
	if m := commitLine.FindStringSubmatch(output); m != nil {
		return m[1]
	}
	return ""
}

var imageLine = regexp.MustCompile(`(?m)^\[Image: ([^\]\n]+)\]\s*$`)

// ImageLine: how a picture attached to a message is named in it, for the agent to read.
func ImageLine(path string) string { return "[Image: " + path + "]" }

func splitImages(text string) (string, []string) {
	var images []string
	for _, m := range imageLine.FindAllStringSubmatch(text, -1) {
		images = append(images, strings.TrimSpace(m[1]))
	}
	if images == nil {
		return text, nil
	}
	return strings.TrimSpace(imageLine.ReplaceAllString(text, "")), images
}

// ProgressOf: the agent's plan from its last TodoWrite, nil when it wrote none.
func ProgressOf(items []Item) *Progress {
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Kind != "tool" || it.Tool != "TodoWrite" {
			continue
		}
		var in struct {
			Todos []struct {
				Content    string `json:"content"`
				Status     string `json:"status"`
				ActiveForm string `json:"activeForm"`
			} `json:"todos"`
		}
		if json.Unmarshal(it.Input, &in) != nil || len(in.Todos) == 0 {
			return nil
		}
		p := &Progress{Total: len(in.Todos)}
		for _, t := range in.Todos {
			p.Steps = append(p.Steps, Todo{Text: t.Content, Status: t.Status})
			switch t.Status {
			case "completed":
				p.Done++
			case "in_progress":
				if p.Current == "" {
					p.Current = t.ActiveForm
					if p.Current == "" {
						p.Current = t.Content
					}
				}
			}
		}
		return p
	}
	return nil
}

// statOf: the turn going on, from your last message.
func statOf(items []Item) TurnStat {
	var st TurnStat
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Kind == "user" {
			st.Since = it.At
			break
		}
		if it.Kind == "tool" {
			st.Steps++
		}
		if st.Since == 0 || (it.At > 0 && it.At < st.Since) {
			st.Since = it.At
		}
	}
	return st
}

// firstLine: a command of several lines (a heredoc) said by its first, marked as cut.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}
