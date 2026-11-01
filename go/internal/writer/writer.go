// Package writer asks Claude Code (haiku, the fast one) to write merge request text from a branch's changes.
package writer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/contextstat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

const maxDiff = 30_000

// Claude is the command used; tests replace it.
var Claude = "claude"

// ReviewText: title and summary of an MR from everything the branch changes against its base.
func ReviewText(ctx context.Context, worktree, baseBranch string, t core.Task) (title, summary string, err error) {
	base, err := gitx.Run(ctx, worktree, "merge-base", "origin/"+baseBranch, "HEAD")
	base = strings.TrimSpace(base)
	if err != nil || base == "" {
		return "", "", fmt.Errorf("немає origin/%s", baseBranch)
	}
	commits, _ := gitx.Run(ctx, worktree, "log", "--format=- %s", base+"..HEAD")
	stat, _ := gitx.Run(ctx, worktree, "diff", "--stat", base, "HEAD")
	diff, _ := gitx.Run(ctx, worktree, "diff", base, "HEAD")
	if strings.TrimSpace(diff) == "" {
		return "", "", errors.New("немає змін")
	}
	text, err := Ask(ctx, worktree, strings.Join([]string{
		"Write the title and description of a merge request for the changes below. Base them on what the diff actually does.",
		"First line: the title, an imperative summary under 72 characters, no issue number.",
		"Then a blank line and 2-6 short bullet points (\"- \"): what changed and why, for a reviewer.",
		fmt.Sprintf("The work is for the issue %q (%s).", t.Title, t.ID),
		"Write them in " + lang() + ".",
		"Answer with the title and bullets only: no headings, no quotes, no code fences, no preface.",
		"", "Commits:", commits, stat, clip(diff),
	}, "\n"))
	if err != nil {
		return "", "", err
	}
	first, rest, _ := strings.Cut(text, "\n")
	title = strings.TrimSpace(titlePrefix.ReplaceAllString(first, ""))
	if len(title) > 120 {
		title = title[:120]
	}
	if title == "" {
		title = t.ID + ": " + t.Title
	}
	return title, strings.TrimSpace(rest), nil
}

var (
	titlePrefix = regexp.MustCompile(`(?i)^#+\s*|^title:\s*`)
	fences      = regexp.MustCompile("^```\\w*\\n?|```\\s*$")
)

// Spend is what one call of Claude Code cost: USD and the tokens it read and wrote (cache included), and how its
// context filled, from the usage of each model call.
type Spend struct {
	USD     float64
	Tokens  int
	Context []core.ContextRun
}

// streamArgs: Claude Code prints each message as it goes (each model call's usage with it) and the result last,
// and keeps no transcript: a call of Mako's is no session to go on, nor the worktree's latest one.
var streamArgs = []string{"--output-format", "stream-json", "--verbose", "--no-session-persistence"}

type spendKey struct{}

// WithSpend: a context whose Claude Code calls tell f what each cost, so the caller can add it to its task.
func WithSpend(ctx context.Context, f func(Spend)) context.Context {
	return context.WithValue(ctx, spendKey{}, f)
}

// maxPlain: the most of what `claude -p` printed besides its JSON lines kept, the answer when there is no result.
const maxPlain = 1 << 20

// output is what `claude -p` printed, read as it came: the result line, the context its calls filled, and any
// text that was no JSON line. The stream itself (each tool result the agent read) is not kept.
type output struct {
	result []byte
	plain  bytes.Buffer
	ctx    contextstat.Result
}

func readOutput(r io.Reader) *output {
	o := &output{}
	stream := contextstat.NewStream()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		var e struct {
			Type   string  `json:"type"`
			Result *string `json:"result"`
		}
		switch {
		case json.Unmarshal(line, &e) != nil || e.Type == "":
			if o.plain.Len() < maxPlain {
				o.plain.Write(sc.Bytes())
				o.plain.WriteByte('\n')
			}
		case e.Type == "result" && e.Result != nil:
			o.result = bytes.Clone(line)
			stream.Line(line)
		default:
			stream.Line(line)
		}
	}
	io.Copy(io.Discard, r) // a line too long to read: the rest is not waited on
	o.ctx = stream.Finish("")
	return o
}

// run runs a `claude -p` and reads what it prints as it goes.
func run(cmd *exec.Cmd) (*output, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	o := readOutput(stdout)
	return o, cmd.Wait()
}

// answer: the text of `claude -p`'s result, telling its spend to the context's WithSpend; output that is not that
// JSON is the text itself. model: the one asked for, the window of its context when the stream did not tell it.
func answer(ctx context.Context, o *output, model string) string {
	text, spend, ok := o.parse()
	if !ok {
		return strings.TrimRight(o.plain.String(), "\n")
	}
	for i := range spend.Context {
		if c := &spend.Context[i]; c.Window == 0 {
			c.Window = core.ContextWindow(model, c.Peak)
		}
	}
	if f, _ := ctx.Value(spendKey{}).(func(Spend)); f != nil && (spend.USD > 0 || spend.Tokens > 0) {
		f(spend)
	}
	return text
}

// ParseResult reads the result `claude -p` prints, its last line with --output-format stream-json: its text and
// what it cost; the stream's model calls tell how the context filled.
func ParseResult(out []byte) (text string, spend Spend, ok bool) {
	return readOutput(bytes.NewReader(out)).parse()
}

func (o *output) parse() (text string, spend Spend, ok bool) {
	var r struct {
		Result string  `json:"result"`
		Cost   float64 `json:"total_cost_usd"`
		Usage  struct {
			Input       int `json:"input_tokens"`
			Output      int `json:"output_tokens"`
			CacheRead   int `json:"cache_read_input_tokens"`
			CacheCreate int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if o.result == nil || json.Unmarshal(o.result, &r) != nil {
		return "", Spend{}, false
	}
	u := r.Usage
	spend = Spend{USD: r.Cost, Tokens: u.Input + u.Output + u.CacheRead + u.CacheCreate}
	for _, run := range o.ctx.Runs {
		run.Window = o.ctx.Window // 0 when the stream did not tell it: answer knows the model
		spend.Context = append(spend.Context, run)
	}
	return r.Result, spend, true
}

// Ask runs `claude -p --model haiku` with the prompt on stdin, for up to two minutes.
func Ask(ctx context.Context, dir, prompt string) (string, error) {
	return AskModel(ctx, dir, "haiku", prompt, 2*time.Minute)
}

// AskModel runs `claude -p --model <model>` with the prompt on stdin, for up to timeout.
func AskModel(ctx context.Context, dir, model, prompt string, timeout time.Duration) (string, error) {
	path, err := exec.LookPath(Claude)
	if err != nil {
		return "", errors.New("Claude Code не знайдено")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, append([]string{"-p", "--model", model}, streamArgs...)...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := run(cmd)
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("Claude не відповів за %d хв", int(timeout.Minutes()))
	}
	if err != nil {
		return "", fmt.Errorf("claude: %v: %s", err, clipTo(strings.TrimSpace(stderr.String()), 200))
	}
	text := strings.TrimSpace(fences.ReplaceAllString(strings.TrimSpace(answer(ctx, out, model)), ""))
	if text == "" || len(text) > 6000 {
		return "", errors.New("Claude не дав відповіді")
	}
	return text, nil
}

func clip(diff string) string {
	if len(diff) > maxDiff {
		return fmt.Sprintf("%s\n… (diff cut at %d characters)", diff[:maxDiff], maxDiff)
	}
	return diff
}

func clipTo(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// TaskSummary: what the task came to, in Mako's language, for the card (and the issue comment): from the agent's last
// words in its terminal, the commits and the diff.
func TaskSummary(ctx context.Context, worktree, baseBranch string, t core.Task, agentText, reviewURL string) (string, error) {
	commits, stat, diff := BranchWork(ctx, worktree, baseBranch)
	agentText = lastOf(agentText, 8000)
	review := "No merge request was opened."
	if reviewURL != "" {
		review = "Merge request: " + reviewURL
	}
	return Ask(ctx, worktree, strings.Join([]string{
		"Summarize, in " + lang() + ", what was done for this task, for the developer who will read it later on a task board.",
		"Use Markdown with these sections, each short (bullets, at most ~6 per section), skipping a section that has nothing:",
		i18n.T("summary.whatDone"),
		i18n.T("summary.howChecked"),
		i18n.T("summary.lookAt") + " (open questions, risks, follow-ups, anything left undone)",
		"Base it on the end of the agent's terminal session and the actual changes below. Do not invent checks that are not mentioned. Answer with the summary only.",
		"", fmt.Sprintf("Task: %s — %s", t.ID, t.Title), review,
		"", "End of the agent's terminal session:", orNone(agentText),
		"", "Commits:", orNone(commits), stat, clip(diff),
	}, "\n"))
}

// BranchWork: the branch's commits, diffstat and diff against its base; empty when there is no base.
func BranchWork(ctx context.Context, worktree, baseBranch string) (commits, stat, diff string) {
	if base, err := gitx.Run(ctx, worktree, "merge-base", "origin/"+baseBranch, "HEAD"); err == nil && base != "" {
		commits, _ = gitx.Run(ctx, worktree, "log", "--format=- %s", base+"..HEAD")
		stat, _ = gitx.Run(ctx, worktree, "diff", "--stat", base, "HEAD")
		diff, _ = gitx.Run(ctx, worktree, "diff", base, "HEAD")
	}
	return commits, stat, diff
}

func lastOf(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// AttemptSummary: a short account of an attempt the developer is dropping, for the next attempt's prompt: the
// approach it took, what it changed, and what went wrong, read against the developer's note.
func AttemptSummary(ctx context.Context, worktree, baseBranch string, t core.Task, agentText, note string) (string, error) {
	commits, stat, diff := BranchWork(ctx, worktree, baseBranch)
	return Ask(ctx, worktree, strings.Join([]string{
		"A coding agent worked on this task, and the developer is dropping its attempt to start over on a fresh branch.",
		"Write, in English, a short account for the agent of the next attempt: at most 6 bullets, no headings.",
		"Cover the approach the attempt took, what it changed (files, key decisions), and what did not work or was left undone;",
		"use the developer's note to tell what went wrong. Do not invent anything not in the material below. Answer with the bullets only.",
		"", fmt.Sprintf("Task: %s — %s", t.ID, t.Title),
		"", "The developer's note on what to do differently:", orNone(note),
		"", "End of the agent's session:", orNone(lastOf(agentText, 8000)),
		"", "Commits:", orNone(commits), stat, clip(diff),
	}, "\n"))
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// Lesson: the rule for the repo's next agents in what you told this one, in one short line; "" when the
// message is no such correction (a request, a question, a «carry on») or CLAUDE.md already says it.
func Lesson(ctx context.Context, dir, message, claudeMD string) (string, error) {
	if len(message) > 8000 {
		message = message[:8000]
	}
	if len(claudeMD) > 8000 {
		claudeMD = claudeMD[:8000]
	}
	text, err := Ask(ctx, dir, strings.Join([]string{
		"A developer wrote the message below to a coding agent working in this repository, in a chat or as review comments.",
		"Decide whether it corrects how the agent works in a way that should hold for every future task in this repository",
		"(a convention, a place not to touch, how tests are written or run here, a tool to use or avoid).",
		"A one-off request about this task only, a question, a «carry on» or praise is not such a correction.",
		"If it is one and CLAUDE.md below does not say it already, answer with the rule only: one imperative sentence,",
		"under 200 characters, in the language of the message, specific enough to follow without this conversation.",
		"Otherwise answer exactly NONE.",
		"", "Message:", message,
		"", "CLAUDE.md now:", orNone(claudeMD),
	}, "\n"))
	if err != nil {
		return "", err
	}
	rule := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(firstNonEmpty(text)), "-*• "))
	rule = strings.Trim(rule, "\"«»")
	if rule == "" || strings.EqualFold(strings.Trim(rule, ". "), "none") || len(rule) > 400 {
		return "", nil
	}
	return rule, nil
}

func firstNonEmpty(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

// Reviewed is what AgentReview answers when the branch has nothing worth fixing.
const Reviewed = "LGTM"

// ReviewerTools: what the reviewer may use; it reads, it never writes.
var ReviewerTools = []string{"Read", "Grep", "Glob", "Bash(ls:*)", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git status:*)"}

// AgentReview: a second agent, with a context of its own, reads the task and the branch's changes (and the
// repo's files as it needs them) and writes its remarks for the agent who did the work; "" when there are
// none. It runs with read-only tools and keeps no session, so the worktree's conversation stays the agent's.
// prior: the earlier rounds' remarks still open, with the agent's answers: the round checks them first.
func AgentReview(ctx context.Context, worktree, baseBranch string, t core.Task, model string, prior []core.Remark) (string, error) {
	commits, stat, diff := BranchWork(ctx, worktree, baseBranch)
	if strings.TrimSpace(diff) == "" {
		return "", errors.New("немає змін")
	}
	description := t.Description
	if len(description) > 8000 {
		description = description[:8000]
	}
	prompt := strings.Join([]string{
		"You review a merge request before a human reviewer sees it. Another agent wrote the code for the task below; you did not.",
		"Read the task and the diff, and open the repository's files (you are in its worktree) when you need more context.",
		fmt.Sprintf("The base branch is origin/%s. Do not change any file: you only write remarks.", baseBranch),
		"Look for what a careful reviewer would point out first: bugs, parts of the task not done, broken edge cases,",
		"missing or wrong tests, leftovers (debug output, commented-out code, unrelated changes), security problems,",
		"and what goes against the repository's conventions (CLAUDE.md, the code around it). Skip style nitpicks and matters of taste.",
	}, "\n") + "\n" + reviewFormat(prior) + "\n" + strings.Join([]string{
		"", "Task: " + t.ID + " — " + t.Title, orNone(description),
		"", "Commits:", orNone(commits), stat, clip(diff),
	}, "\n")
	path, err := exec.LookPath(Claude)
	if err != nil {
		return "", errors.New("Claude Code не знайдено")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	args := append([]string{"-p"}, streamArgs...)
	args = append(args, "--permission-mode", "default", "--disallowedTools", "Edit", "Write", "NotebookEdit", "--allowedTools")
	args = append(args, ReviewerTools...)
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = worktree
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = append(os.Environ(), "MAKO_REVIEWER=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := run(cmd)
	if ctx.Err() == context.DeadlineExceeded {
		return "", errors.New("рецензент не відповів за 15 хв")
	}
	if err != nil {
		return "", fmt.Errorf("claude: %v: %s", err, clipTo(strings.TrimSpace(stderr.String()), 200))
	}
	return ReviewNotes(answer(ctx, out, model))
}

// reviewFormat: how the reviewer writes its answer, so that Mako can number and track the remarks; with prior,
// the earlier remarks it checks first.
func reviewFormat(prior []core.Remark) string {
	lines := []string{
		"Write in " + lang() + ", but keep these markers in English exactly as shown. One line per remark, nothing else:",
		"R<n> [must] path/to/file.go:42 — what to change and why",
		"R<n> [should] path/to/file.go — what to change and why",
		"[must]: a bug, a part of the task not done, a broken case, a security problem — it has to be fixed before the merge.",
		"[should]: worth fixing, but the merge does not depend on it. Number your remarks from R1; Mako renumbers them.",
	}
	if len(prior) == 0 {
		return strings.Join(append(lines, "If there is nothing worth fixing, answer exactly "+Reviewed+"."), "\n")
	}
	return strings.Join(append(lines,
		"",
		"This is not the first round. The earlier rounds left the remarks below; the agent has worked on them since and answered.",
		"Check each of them first against the code as it is now, one line each, before any new remark:",
		"R<n>: closed — it is fixed",
		"R<n>: open — why it is still a problem",
		"R<n>: accepted — the agent's explanation is good enough",
		"New remarks only about what changed since, or real problems ([must]): do not bring up new matters of taste, so that the review converges.",
		"If every earlier remark is closed or accepted and there is nothing new, answer exactly "+Reviewed+".",
		"", "Earlier remarks and the agent's answers:", PriorText(prior),
	), "\n")
}

// ReviewNotes: the reviewer's remarks from its answer, "" when it found nothing to fix.
func ReviewNotes(answer string) (string, error) {
	text := strings.TrimSpace(fences.ReplaceAllString(strings.TrimSpace(answer), ""))
	if text == "" {
		return "", errors.New("рецензент не дав відповіді")
	}
	if strings.EqualFold(strings.Trim(text, " .!*`\"«»"), Reviewed) {
		return "", nil
	}
	if len(text) > 12000 {
		text = text[:12000] + "\n…"
	}
	return text, nil
}

// Assess: the agent's rating of a finished task, from the task as it was written, the end of the agent's work
// and the branch's changes. Each score is from 1 to 5, its reason and what the description lacked in Mako's language.
func Assess(ctx context.Context, dir, baseBranch string, t core.Task, agentText, summary string) (core.Assessment, error) {
	var commits, stat, diff string
	if baseBranch != "" {
		commits, stat, diff = BranchWork(ctx, dir, baseBranch)
	}
	var comments []string
	for _, c := range t.Comments {
		comments = append(comments, "- "+c.Author+": "+clipTo(c.Body, 1500))
	}
	text, err := Ask(ctx, dir, strings.Join([]string{
		"A coding agent finished the task below. Rate it, for the developer who gave the task, on four scores from 1 to 5:",
		"- clarity: how well the task was described: 5 when it said everything needed to do it (what, where, how to tell it is done), 1 when the agent had to guess most of it;",
		"- complexity: how hard the task was: 1 a trivial edit, 5 a large or tricky change across many places;",
		"- completeness: how much of what the task asks the changes actually do: 5 all of it, 1 almost nothing;",
		"- risk: how likely the change is to break something or need rework in review: 1 safe, 5 very risky.",
		"Also the size the task came to: \"small\" (a few lines in one place), \"medium\" (several files of one part), or \"big\" (many parts, new subsystems, migrations).",
		"Base the scores on the task's text and the actual changes, not on what the agent says about itself.",
		"Answer with JSON only, no code fences, in this shape, each \"why\" one short sentence in " + lang() + ":",
		`{"clarity":{"score":1,"why":"..."},"complexity":{"score":1,"why":"..."},"completeness":{"score":1,"why":"..."},"risk":{"score":1,"why":"..."},"size":"small","missing":"...","review":{"need":false,"why":"..."}}`,
		"\"missing\": in " + lang() + ", at most 3 short points on what the description should have said (\"\" when nothing).",
		"\"review\": your advice to the developer whether a review (by them or a reviewer agent) is worth it before the merge: \"need\" true when the change is risky, subtle or touches what is easy to break, false when it is plain and safe; \"why\" one short sentence in " + lang() + ".",
		"", "Task: " + t.ID + " — " + t.Title, orNone(t.Meta), orNone(clipTo(t.Description, 8000)),
		"", "Comments on the task:", orNone(strings.Join(comments, "\n")),
		"", "Summary of the work:", orNone(summary),
		"", "End of the agent's session:", orNone(lastOf(agentText, 6000)),
		"", "Commits:", orNone(commits), stat, clip(diff),
	}, "\n"))
	if err != nil {
		return core.Assessment{}, err
	}
	return ParseAssessment(text)
}

// ParseAssessment reads the JSON Assess asks for; a score out of 1..5 is an error, not a guess.
func ParseAssessment(answer string) (core.Assessment, error) {
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end < start {
		return core.Assessment{}, errors.New("оцінка не в JSON")
	}
	type score struct {
		Score int    `json:"score"`
		Why   string `json:"why"`
	}
	var in struct {
		Clarity, Complexity, Completeness, Risk score
		Size                                    string
		Missing                                 json.RawMessage
		Review                                  *struct {
			Need bool   `json:"need"`
			Why  string `json:"why"`
		}
	}
	if err := json.Unmarshal([]byte(answer[start:end+1]), &in); err != nil {
		return core.Assessment{}, fmt.Errorf("оцінка не в JSON: %v", err)
	}
	a := core.Assessment{State: "done", Missing: pointsText(in.Missing)}
	// The size came later than the scores: one it does not know is left out, not a failed assessment.
	if size := strings.ToLower(strings.TrimSpace(in.Size)); slices.Contains(core.Sizes, size) {
		a.Size = size
	}
	// The advice came later too: an answer without it is still an assessment.
	if in.Review != nil {
		a.Review = &core.ReviewAdvice{Need: in.Review.Need, Why: clipTo(strings.TrimSpace(in.Review.Why), 500)}
	}
	for _, s := range []struct {
		in  score
		out *core.Score
	}{{in.Clarity, &a.Clarity}, {in.Complexity, &a.Complexity}, {in.Completeness, &a.Completeness}, {in.Risk, &a.Risk}} {
		if s.in.Score < 1 || s.in.Score > 5 {
			return core.Assessment{}, fmt.Errorf("оцінка %d поза 1–5", s.in.Score)
		}
		*s.out = core.Score{Value: s.in.Score, Why: clipTo(strings.TrimSpace(s.in.Why), 500)}
	}
	return a, nil
}

// pointsText: a few points, as a string or, as models sometimes answer, a list of them.
func pointsText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return clipTo(strings.TrimSpace(s), 1500)
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		var out []string
		for _, l := range list {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, "- "+strings.TrimLeft(l, "-*• "))
			}
		}
		return clipTo(strings.Join(out, "\n"), 1500)
	}
	return ""
}

// Retro: the look back at a merged task, from its story as Mako saw it (the task, the attempts, your messages
// to the agent, the reviews, the journal). Each part is a few points in Mako's language.
func Retro(ctx context.Context, dir string, t core.Task, story string) (core.Retro, error) {
	text, err := Ask(ctx, dir, strings.Join([]string{
		"A coding agent did the task below and its merge request is merged. Write a short retrospective of how the task went,",
		"for the developer who gave it, so the next tasks go better. Three parts, each at most 4 short points in " + lang() + ":",
		"- good: what went well;",
		"- bad: what went wrong, was redone, or took longer than it should (retries, corrections, review rounds, failures);",
		"- next: what to do differently next time: in how the task is written, in what the agent is told, in review.",
		"Base it only on the story below; do not invent events. A part with nothing to say is [].",
		"Answer with JSON only, no code fences, in this shape:",
		`{"good":["..."],"bad":["..."],"next":["..."]}`,
		"", "Task: " + t.ID + " — " + t.Title, orNone(t.Meta), orNone(clipTo(t.Description, 6000)),
		"", story,
	}, "\n"))
	if err != nil {
		return core.Retro{}, err
	}
	return ParseRetro(text)
}

// ParseRetro reads the JSON Retro asks for; an answer with nothing in it is an error.
func ParseRetro(answer string) (core.Retro, error) {
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end < start {
		return core.Retro{}, errors.New("ретро не в JSON")
	}
	var in struct{ Good, Bad, Next json.RawMessage }
	if err := json.Unmarshal([]byte(answer[start:end+1]), &in); err != nil {
		return core.Retro{}, fmt.Errorf("ретро не в JSON: %v", err)
	}
	r := core.Retro{State: "done", Good: pointsText(in.Good), Bad: pointsText(in.Bad), Next: pointsText(in.Next)}
	if r.Good == "" && r.Bad == "" && r.Next == "" {
		return core.Retro{}, errors.New("ретро порожнє")
	}
	return r, nil
}

// RepoOutline: the repository's layout for an estimate, without its code: the folders with how many files each
// holds, and the start of README.md and CLAUDE.md; "" when dir is no git repository.
func RepoOutline(ctx context.Context, dir string) string {
	files, err := gitx.Run(ctx, dir, "ls-files")
	if err != nil || strings.TrimSpace(files) == "" {
		return ""
	}
	counts, order := map[string]int{}, []string{}
	for _, f := range strings.Split(strings.TrimSpace(files), "\n") {
		d := "."
		if i := strings.LastIndex(f, "/"); i > 0 {
			d = f[:i]
		}
		if counts[d] == 0 {
			order = append(order, d)
		}
		counts[d]++
	}
	var lines []string
	for i, d := range order {
		if i == 300 {
			lines = append(lines, fmt.Sprintf("… and %d more folders", len(order)-300))
			break
		}
		lines = append(lines, fmt.Sprintf("%s/ (%d files)", d, counts[d]))
	}
	out := []string{"Folders (files in each):", strings.Join(lines, "\n")}
	for _, name := range []string{"README.md", "CLAUDE.md"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			out = append(out, "", name+":", clipTo(string(b), 3000))
		}
	}
	return strings.Join(out, "\n")
}

// EstimateModel: the model that estimates a task and so picks who does it; EstimateTimeout: how long it may take.
const (
	EstimateModel   = "opus"
	EstimateTimeout = 5 * time.Minute
)

// Estimate: the agent's look at a new task before it starts, from its text and the repository's layout (not its
// code): how clear it is and what to ask, how big, how risky, and how to take it. Its text is in Mako's language.
func Estimate(ctx context.Context, dir string, t core.Task, outline string) (core.Estimate, error) {
	var comments []string
	for _, c := range t.Comments {
		comments = append(comments, "- "+c.Author+": "+clipTo(c.Body, 1500))
	}
	text, err := AskModel(ctx, dir, EstimateModel, strings.Join([]string{
		"A developer is deciding whether to give the task below to a coding agent, and how. Before anyone starts, estimate it",
		"from its text and the repository's layout (you do not see the code). Answer with JSON only, no code fences, in this shape:",
		`{"clarity":{"score":1,"why":"..."},"questions":["..."],"size":"small","parts":"...","risk":{"score":1,"why":"..."},"plan":false,"split":false,"model":"","advice":"..."}`,
		"- clarity: 5 when the task says all that is needed (what, where, how to tell it is done), 1 when the agent would have to guess most of it;",
		"- questions: at most 4 concrete questions to answer in the tracker first, only for what is really missing ([] when nothing);",
		"- size: \"small\" (a few lines in one place), \"medium\" (several files of one part), or \"big\" (many parts, new subsystems, migrations);",
		"- parts: which folders or parts of the code it will likely touch, one short line;",
		"- risk: 1 safe, 5 likely to break something; \"why\" names what it touches (settings, updates, stored data, security, the UI, …);",
		"- plan: true when the agent should first plan and have the plan approved; split: true when it is several tasks in one;",
		"- model: who should do it, by how complex it is: \"haiku\" for a trivial edit in one place, \"sonnet\" for ordinary work",
		"  (the default), \"opus\" only for complex work: across subsystems, subtle logic, hard to get right;",
		"- advice: one short sentence on how to take the task.",
		"All text (why, questions, parts, advice) in " + lang() + ", short.",
		"", "Task: " + t.ID + " — " + t.Title, orNone(t.Meta), orNone(clipTo(t.Description, 8000)),
		"", "Comments on the task:", orNone(strings.Join(comments, "\n")),
		"", "Repository:", orNone(outline),
	}, "\n"), EstimateTimeout)
	if err != nil {
		return core.Estimate{}, err
	}
	return ParseEstimate(text)
}

// ParseEstimate reads the JSON Estimate asks for; a score out of 1..5 or an unknown size is an error.
func ParseEstimate(answer string) (core.Estimate, error) {
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end < start {
		return core.Estimate{}, errors.New("оцінка не в JSON")
	}
	type score struct {
		Score int    `json:"score"`
		Why   string `json:"why"`
	}
	var in struct {
		Clarity, Risk      score
		Questions          json.RawMessage
		Size, Parts, Model string
		Advice             string
		Plan, Split        bool
	}
	if err := json.Unmarshal([]byte(answer[start:end+1]), &in); err != nil {
		return core.Estimate{}, fmt.Errorf("оцінка не в JSON: %v", err)
	}
	e := core.Estimate{
		State: "done", Questions: pointsText(in.Questions), Size: strings.ToLower(strings.TrimSpace(in.Size)),
		Parts: clipTo(strings.TrimSpace(in.Parts), 500), Plan: in.Plan, Split: in.Split,
		Model: strings.ToLower(strings.TrimSpace(in.Model)), Advice: clipTo(strings.TrimSpace(in.Advice), 500),
	}
	if !slices.Contains(core.Sizes, e.Size) {
		return core.Estimate{}, fmt.Errorf("невідомий розмір %q", in.Size)
	}
	if !slices.Contains(core.Models, e.Model) {
		e.Model = "" // no advice: the agent starts with the default one
	}
	for _, s := range []struct {
		in  score
		out *core.Score
	}{{in.Clarity, &e.Clarity}, {in.Risk, &e.Risk}} {
		if s.in.Score < 1 || s.in.Score > 5 {
			return core.Estimate{}, fmt.Errorf("оцінка %d поза 1–5", s.in.Score)
		}
		*s.out = core.Score{Value: s.in.Score, Why: clipTo(strings.TrimSpace(s.in.Why), 500)}
	}
	return e, nil
}

// lang: the language Mako speaks, as the agent is asked to write in it.
func lang() string { return i18n.EnglishName(i18n.Lang()) }
