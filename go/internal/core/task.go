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
	Cost           float64       `json:"cost,omitempty"` // USD Mako's own agent spent on the task, over all attempts
	// View: how Mako shows its own agent, "chat" or "terminal"; the same conversation either way.
	View string `json:"view,omitempty"`
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
