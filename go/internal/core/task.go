// Package core holds what every part of Mako talks about: a tracker's task and Mako's record of it.
package core

// Task is an issue as Mako sees it, whatever tracker it came from. The JSON names match the TypeScript
// Mako's state.json, so both read the same file.
type Task struct {
	ID          string       `json:"id"`        // "group/app#12", "PROJ-12"
	Slug        string       `json:"slug"`      // "app-12", "PROJ-12": safe for folders
	TrackerID   string       `json:"trackerId"` // the tracker's id in the config
	Project     string       `json:"project"`   // "group/app", "PROJ"
	Title       string       `json:"title"`
	Description string       `json:"description"`
	URL         string       `json:"url"`
	Meta        string       `json:"meta,omitempty"` // "Type: Bug | Priority: High"
	Labels      []string     `json:"labels"`
	Components  []string     `json:"components"`
	Comments    []Comment    `json:"comments"`
	ClosingRef  string       `json:"closingRef,omitempty"` // "Closes group/app#12"
	NativeID    string       `json:"nativeId,omitempty"`   // GitLab's global issue id
	Assignees   []string     `json:"assignees,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
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
	Error          string        `json:"error,omitempty"`
	Summary        string        `json:"summary,omitempty"`   // what the agent did, written when its MR appeared
	SessionID      string        `json:"sessionId,omitempty"` // Mako's own agent: its Claude Code session
	Attempt        int           `json:"attempt,omitempty"`
	// «Повторити інакше»: what to do differently this attempt, what the last one came to, and the model for it.
	RetryNote   string  `json:"retryNote,omitempty"`
	LastAttempt string  `json:"lastAttempt,omitempty"`
	Model       string  `json:"model,omitempty"` // "" — the configured one
	Cost        float64 `json:"cost,omitempty"`  // USD Mako's own agent spent on the task, over all attempts
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
	// AgentReviews: the rounds of the reviewer agent over the branch, the oldest first.
	AgentReviews []AgentReview `json:"agentReviews,omitempty"`
	// Assessment: how the agent rates the finished task: its description, how hard it was, how much of it is
	// done and how risky the change is.
	Assessment *Assessment `json:"assessment,omitempty"`
	// Retro: the look back at the task once its MR is merged: what went well, what did not, what to do
	// differently next time.
	Retro *Retro `json:"retro,omitempty"`
}

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
	// Missing: what the description should have said, for whoever writes the next one.
	Missing string `json:"missing,omitempty"`
	Error   string `json:"error,omitempty"`
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
	// State: "running"; "notes" (its remarks went to the agent); "clean" (nothing to fix); "failed"; "skipped"
	// (the task's risk is below what the settings ask the reviewer for: Reason says which).
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Notes  string `json:"notes,omitempty"` // Markdown
	Error  string `json:"error,omitempty"`
}

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
