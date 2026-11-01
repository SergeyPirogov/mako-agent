// Package core holds what every part of Mako talks about: a tracker's task and Mako's record of it.
package core

import (
	"strings"
	"unicode"

	"github.com/SergeyPirogov/mako-agent/go/internal/secrets"
)

// Task is an issue as Mako sees it, whatever tracker it came from. The JSON names match the TypeScript
// Mako's state.json, so both read the same file.
type Task struct {
	ID          string `json:"id"`        // "group/app#12", "PROJ-12"
	Slug        string `json:"slug"`      // "app-12", "PROJ-12": safe for folders
	TrackerID   string `json:"trackerId"` // the tracker's id in the config
	Project     string `json:"project"`   // "group/app", "PROJ"
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Meta        string `json:"meta,omitempty"` // "Type: Bug | Priority: High"
	// Priority: the tracker's priority of the issue ("High", "P1"); "" when it has none.
	Priority    string       `json:"priority,omitempty"`
	Labels      []string     `json:"labels"`
	Components  []string     `json:"components"`
	Comments    []Comment    `json:"comments"`
	ClosingRef  string       `json:"closingRef,omitempty"` // "Closes group/app#12"
	NativeID    string       `json:"nativeId,omitempty"`   // GitLab's global issue id
	Assignees   []string     `json:"assignees,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	// Author: who wrote the issue in the tracker ("" unknown).
	Author string `json:"author,omitempty"`
	// CommentsUnread: the tracker failed to give its comments this time; the ones Mako has stay.
	CommentsUnread bool `json:"-"`
}

// Plain: the words of a text, lowercased, without the Markdown and the spacing a tracker may change on its way
// back (Jira's ADF): to tell whether two texts say the same.
func Plain(text string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

type Comment struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

type Attachment struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Where string `json:"where"`
}

// Status of a task in Mako.
//
//	asking, snoozed – new: waiting for «Взяти в роботу» (snoozed with a time ahead: postponed)
//	handed-off      – in Orca, waiting for its MR
//	done            – the MR is open (in review) or merged
//	failed, cancelled
type Status string

const (
	Asking    Status = "asking"
	Snoozed   Status = "snoozed"
	Queued    Status = "queued"
	HandedOff Status = "handed-off"
	Done      Status = "done"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
)

// Record is Mako's memory of one task: where it is and what happened to it.
type Record struct {
	ID             string        `json:"id"`
	Status         Status        `json:"status"`
	Task           Task          `json:"task"`
	Repo           string        `json:"repo,omitempty"`
	Executor       string        `json:"executor,omitempty"`
	UpdatedAt      string        `json:"updatedAt"`
	SnoozedUntil   int64         `json:"snoozedUntil,omitempty"`
	PostponedByYou bool          `json:"postponedByYou,omitempty"`
	StartedAt      string        `json:"startedAt,omitempty"`
	FinishedAt     string        `json:"finishedAt,omitempty"`
	Worktree       string        `json:"worktree,omitempty"`
	Branch         string        `json:"branch,omitempty"`
	BranchName     string        `json:"branchName,omitempty"`
	PRURL          string        `json:"prUrl,omitempty"`
	ReviewState    string        `json:"reviewState,omitempty"` // "merged", "closed"
	MergedAt       string        `json:"mergedAt,omitempty"`
	ArchivedAt     int64         `json:"archivedAt,omitempty"`
	Gone           string        `json:"gone,omitempty"` // "closed", "deleted": in the tracker
	TrackerStatus  *TrackerState `json:"trackerStatus,omitempty"`
	// AwaitingCI: the MR's CI failed when it appeared; the issue moves to review (with the comment) once CI passes.
	AwaitingCI bool   `json:"awaitingCi,omitempty"`
	Error      string `json:"error,omitempty"`
	// WorktreeKept: the uncommitted files that kept the worktree of a finished task from being cleaned up;
	// the board asks whether to remove it anyway.
	WorktreeKept int    `json:"worktreeKept,omitempty"`
	Summary      string `json:"summary,omitempty"`   // what the agent did, written when its MR appeared
	SessionID    string `json:"sessionId,omitempty"` // Mako's own agent: its Claude Code session
	Attempt      int    `json:"attempt,omitempty"`
	// «Повторити інакше»: what to do differently this attempt, what the last one came to, and the model for it.
	RetryNote   string `json:"retryNote,omitempty"`
	LastAttempt string `json:"lastAttempt,omitempty"`
	Model       string `json:"model,omitempty"` // "" — the configured one (or the estimate's, when the settings say)
	// AgentModel: the model the agent last worked with: Claude Code's own name of it when its chat reported one,
	// else the one Mako asked for ("opus"); "" unknown (the agent's default).
	AgentModel string `json:"agentModel,omitempty"`
	// PlanFirst: «план спочатку» picked on the card from the estimate: the agent starts with a plan.
	PlanFirst bool    `json:"planFirst,omitempty"`
	Cost      float64 `json:"cost,omitempty"` // USD Mako's own agent spent on the task, over all attempts
	// View: how Mako shows its own agent, "chat" or "terminal"; the same conversation either way.
	View string `json:"view,omitempty"`
	// Planning: the agent started in plan mode and its plan is not approved yet; it starts in plan mode again
	// until it is.
	Planning bool `json:"planning,omitempty"`
	// Lesson: a rule for CLAUDE.md Mako drew from your last correction of the agent, until you save it or say no.
	Lesson *Lesson `json:"lesson,omitempty"`
	// AutoFixed: what of the MR the agent was already asked to fix by itself, per kind ("ci", "conflicts",
	// "comments"), so the same problem is asked about once; AutoFixes counts those requests.
	AutoFixed map[string]string `json:"autoFixed,omitempty"`
	AutoFixes int               `json:"autoFixes,omitempty"`
	// BaseConflict: the branch conflicted with its base branch and the agent was asked to resolve it; until
	// the branch has that commit of the base.
	BaseConflict *BaseConflict `json:"baseConflict,omitempty"`
	// ReviewFixes: how many times the agent was asked to fix the MR's review comments, by you or by itself.
	ReviewFixes int `json:"reviewFixes,omitempty"`
	// CIFixes: how many times the agent was asked to fix the MR's CI, by you or by itself.
	CIFixes int `json:"ciFixes,omitempty"`
	// Metrics: where the task's time went, by stage, and what each phase of it cost; kept as it goes.
	Metrics *Metrics `json:"metrics,omitempty"`
	// AgentReviews: the rounds of the reviewer agent over the branch, the oldest first.
	AgentReviews []AgentReview `json:"agentReviews,omitempty"`
	// Assessment: how the agent rates the finished task: its description, how hard it was, how much of it is
	// done and how risky the change is.
	Assessment *Assessment `json:"assessment,omitempty"`
	// Retro: the look back at the task once its MR is merged: what went well, what did not, what to do
	// differently next time.
	Retro *Retro `json:"retro,omitempty"`
	// Estimate: the agent's look at the task before it starts: how clear, how big, how risky, and how to take it.
	Estimate *Estimate `json:"estimate,omitempty"`
	// AutoSkip: why «Віддавати нові задачі без питань» left this new task in «Нові» («велика», «є питання до
	// опису», …); "" when it did not. Mako looks again each poll, so a task taken once its estimate fits.
	AutoSkip string `json:"autoSkip,omitempty"`
	// Secrets: what the last push Mako stopped found in the branch's changes; empty once a push passes.
	// SecretsAllowed: the lines «Це не секрет» let through (secrets.Finding.Key).
	Secrets        []secrets.Finding `json:"secrets,omitempty"`
	SecretsAllowed []string          `json:"secretsAllowed,omitempty"`
	// Checks: the repo's checks (tests, lint, build) Mako ran in the worktree before opening the MR, the last run.
	Checks *Checks `json:"checks,omitempty"`
}

// BaseConflict: the base branch's commit the task's branch conflicted with, and the files.
type BaseConflict struct {
	Base  string   `json:"base"`
	Files []string `json:"files"`
}

// Checks is the last run of a repo's checks over the agent's branch, before Mako opens its MR.
type Checks struct {
	At     string `json:"at"`
	Commit string `json:"commit"` // the HEAD they ran over
	// State: "running", "passed" or "failed".
	State   string        `json:"state"`
	Results []CheckResult `json:"results,omitempty"`
	// Fails: the runs that failed one after another; at the settings' limit the agent is not asked again and
	// the task needs you (Attention).
	Fails     int  `json:"fails,omitempty"`
	Attention bool `json:"attention,omitempty"`
	// Error: why the MR was not opened after the checks passed (the push or the code host failed).
	Error string `json:"error,omitempty"`
}

// CheckResult is one command of the checks.
type CheckResult struct {
	Name     string `json:"name"` // "тести", "лінт", "збірка", or the command
	Command  string `json:"command"`
	OK       bool   `json:"ok"`
	TimedOut bool   `json:"timedOut,omitempty"`
	Seconds  int    `json:"seconds,omitempty"`
	// Tail: the last lines of what a failed command printed.
	Tail string `json:"tail,omitempty"`
}

// Estimate is the agent's rating of a new task, from its text and the repository's layout, before anyone
// takes it.
type Estimate struct {
	At string `json:"at"`
	// State: "running", "done" or "failed".
	State string `json:"state"`
	// For: the key of the title and description it was made for; another key means the task changed since.
	For string `json:"for,omitempty"`
	// Clarity: how well the task is described (5: all that is needed is in it).
	Clarity Score `json:"clarity"`
	// Questions: what to ask in the tracker before the agent starts, Markdown points ("" none).
	Questions string `json:"questions,omitempty"`
	// Size: "small", "medium" or "big".
	Size string `json:"size,omitempty"`
	// Parts: which parts of the code it will likely touch.
	Parts string `json:"parts,omitempty"`
	// Risk: how likely it is to break something (5: the riskiest), with what it touches.
	Risk Score `json:"risk"`
	// Plan, Split, Model: start with a plan, split into several tasks, the model to give it ("" any).
	Plan   bool   `json:"plan,omitempty"`
	Split  bool   `json:"split,omitempty"`
	Model  string `json:"model,omitempty"`
	Advice string `json:"advice,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Sizes a task can be, the smallest first: what the estimate expects and what the finished task came to.
var Sizes = []string{"small", "medium", "big"}

// Models: the agent's models to pick from (the estimate's advice, «Взяти в роботу», «Повторити інакше»).
var Models = []string{"opus", "sonnet", "haiku"}

// Retro is the agent's retrospective of a merged task, each part a few Markdown bullets in Ukrainian.
type Retro struct {
	At string `json:"at"`
	// State: "running", "done" or "failed".
	State string `json:"state"`
	Good  string `json:"good,omitempty"` // what went well
	Bad   string `json:"bad,omitempty"`  // what went wrong or took longer than it should
	Next  string `json:"next,omitempty"` // what to do differently next time: in the task's text, for the agent, in review
	Error string `json:"error,omitempty"`
}

// Assessment is the agent's rating of a finished task, each score from 1 to 5.
type Assessment struct {
	At string `json:"at"`
	// State: "running", "done" or "failed".
	State string `json:"state"`
	// Clarity: how well the task was described (5: everything needed was in it).
	Clarity Score `json:"clarity"`
	// Complexity: how hard the task was (5: the hardest).
	Complexity Score `json:"complexity"`
	// Completeness: how much of the task the change does (5: all of it).
	Completeness Score `json:"completeness"`
	// Risk: how likely the change is to break something (5: the riskiest).
	Risk Score `json:"risk"`
	// Size: what the task came to, "small", "medium" or "big" ("" not rated: an older assessment).
	Size string `json:"size,omitempty"`
	// Missing: what the description should have said, for whoever writes the next one.
	Missing string `json:"missing,omitempty"`
	// Review: whether the agent advises a review before the merge, and why; nil from older versions of Mako
	// (the board goes by the risk then). You decide: «Робити» or «Пропустити».
	Review *ReviewAdvice `json:"review,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// ReviewAdvice is the agent's say on whether the change needs a review before the merge.
type ReviewAdvice struct {
	Need bool   `json:"need"`
	Why  string `json:"why,omitempty"`
}

// Score is one rating from 1 to 5 (0: none) with why.
type Score struct {
	Value int    `json:"value"`
	Why   string `json:"why,omitempty"`
}

// AgentReview is one look of the reviewer agent at the task's branch.
type AgentReview struct {
	At     string `json:"at"`
	Commit string `json:"commit"` // the branch's HEAD it read
	// State: "running"; "notes" (it has remarks); "clean" (nothing to fix); "failed"; "skipped" (older versions
	// of Mako: the task's risk was below what the settings asked the reviewer for; Reason says which).
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Notes  string `json:"notes,omitempty"` // Markdown
	Error  string `json:"error,omitempty"`
	// Sent: the remarks went to the agent, by your «Виправити».
	Sent bool `json:"sent,omitempty"`
	// Remarks: the round's numbered remarks (R1, R2… across the task's rounds); none in rounds of older versions
	// of Mako or when the reviewer did not keep to the format: then Notes is all there is.
	Remarks []Remark `json:"remarks,omitempty"`
	// Checks: what this round says of the earlier rounds' remarks, after the agent's fixes.
	Checks []Check `json:"checks,omitempty"`
	// SentHead: the branch's HEAD when «Виправити» handed the remarks over, to tell whether the agent committed.
	SentHead string `json:"sentHead,omitempty"`
	// Reply: the agent's report on the remarks, as it wrote it; ReplyAt when Mako read it.
	Reply   string `json:"reply,omitempty"`
	ReplyAt string `json:"replyAt,omitempty"`
	// Asked: the agent ended its turn with no report, and Mako asked it once to write one.
	Asked bool `json:"asked,omitempty"`
	// Unverified: the agent says it fixed some remark, but the branch has no new commit since «Виправити».
	Unverified bool `json:"unverified,omitempty"`
	// The comments Mako left in the tracker: the remarks in the issue and in the MR, the agent's report.
	CommentURL   string `json:"commentUrl,omitempty"`
	MRCommentURL string `json:"mrCommentUrl,omitempty"`
	ReplyURL     string `json:"replyUrl,omitempty"`
	MRReplyURL   string `json:"mrReplyUrl,omitempty"`
	// Escalated: the rounds ran out with remarks still open: the decision is a person's now.
	Escalated bool `json:"escalated,omitempty"`
}

// A Remark is one numbered remark of the reviewer, with what the agent answered on it.
type Remark struct {
	ID     string `json:"id"`     // "R1"
	Weight string `json:"weight"` // "must" or "should"
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Text   string `json:"text"`
	// Reply: the agent's "fixed", "not" (not fixed, ReplyNote says why) or "partial"; "" with no report yet.
	Reply     string `json:"reply,omitempty"`
	ReplyNote string `json:"replyNote,omitempty"`
	// Again: an earlier round's remark this round found still open, carried over for the agent once more.
	Again bool `json:"again,omitempty"`
}

// A Check is the next round's verdict on an earlier remark: "closed", "open" or "accepted" (the explanation).
type Check struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
	Note    string `json:"note,omitempty"`
}

// Remark weights, replies and verdicts.
const (
	Must         = "must"
	Should       = "should"
	Fixed        = "fixed"
	NotFixed     = "not"
	Partly       = "partial"
	Closed       = "closed"
	Open         = "open"
	Accepted     = "accepted"
	ReviewRounds = 2 // the reviewer's rounds over a task before a person decides
)

// Resolved: the verdict closes the remark.
func (c Check) Resolved() bool { return c.Verdict == Closed || c.Verdict == Accepted }

// Lesson is a rule for the repo's next agents, drawn from what you told one of them.
type Lesson struct {
	Rule string `json:"rule"`
	From string `json:"from"` // what you wrote, clipped
}

// SavedLesson is a rule that went into a repo's CLAUDE.md, for the project's settings.
type SavedLesson struct {
	TrackerID string `json:"trackerId"`
	Project   string `json:"project"`
	Repo      string `json:"repo"`
	Rule      string `json:"rule"`
	Task      string `json:"task"`
	Branch    string `json:"branch"` // where it was committed
	Commit    string `json:"commit"`
	At        string `json:"at"`
}

// TrackerState is what happened to the issue's status at the last step, shown on the board.
type TrackerState struct {
	OK   bool   `json:"ok"`
	Text string `json:"text"`
}

// Column is where the board shows a record.
func (r *Record) Column(now int64) string {
	switch r.Status {
	case Asking, Snoozed:
		return "new"
	case Queued, HandedOff:
		return "doing"
	case Failed, Cancelled:
		return "failed"
	}
	if r.ReviewState == "closed" {
		return "failed"
	}
	if r.PRURL != "" && r.ReviewState == "" {
		return "review"
	}
	return "done"
}

// Postponed: «Відкласти» was pressed and its time has not come; an unanswered notification is still new.
func (r *Record) Postponed(nowMs int64) bool {
	return r.Status == Snoozed && r.PostponedByYou && r.SnoozedUntil > nowMs
}
