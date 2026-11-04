package core

import "time"

// ReviewRequest is a merge request of someone else where I am a reviewer, shown on the board under
// «Рев'ю для мене» next to my own tasks.
type ReviewRequest struct {
	ID        string `json:"id"` // the MR's URL: one card per MR, whatever the search that found it
	TrackerID string `json:"trackerId"`
	Project   string `json:"project"` // "group/app"
	Number    int    `json:"number"`
	// Ref: how the code host names it, "!12" (GitLab) or "#12" (GitHub).
	Ref         string `json:"ref"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Author      string `json:"author"`
	Additions   int    `json:"additions"`
	Deletions   int    `json:"deletions"`
	CI          string `json:"ci,omitempty"` // as trackers.ReviewCheck.CI
	HeadSHA     string `json:"headSha,omitempty"`
	// Approved: I approved it (the tracker's answer, not kept): the card goes to «Готово».
	Approved bool `json:"-"`

	// What Mako keeps of it.
	// Since: when Mako first saw it waiting for me.
	Since string `json:"since"`
	// State: "open"; "done" once it is merged, closed or approved by me (Done says which).
	State string `json:"state"`
	Done  string `json:"done,omitempty"` // "merged", "closed", "approved"
	// DoneAt: when it went to «Готово».
	DoneAt string `json:"doneAt,omitempty"`
	// Updated: new commits since the card was last opened.
	Updated bool `json:"updated,omitempty"`
	// Draft: what Mako's reviewer made of the MR at «Підготувати ревʼю», the comments to send among it.
	Draft *ReviewDraft `json:"draft,omitempty"`
}

// ReviewDraft is the reviewer agent's look at someone else's MR, for me to review it: what it changes and why,
// its risks as comments to send to the MR, what to check by hand, whether to merge it.
type ReviewDraft struct {
	// State: "running"; "ready" (the reviewer answered); "failed" (Error says why).
	State  string `json:"state"`
	At     string `json:"at"`
	Commit string `json:"commit,omitempty"` // the MR's head it read
	Error  string `json:"error,omitempty"`
	// Limited: there is no clone of the project on this Mac: the reviewer read the diff only.
	Limited bool `json:"limited,omitempty"`
	// Stale: the MR has new commits since the head the reviewer read (Commit): «змінилось після ревʼю».
	Stale   bool   `json:"stale,omitempty"`
	Summary string `json:"summary,omitempty"` // Markdown
	// Verdict: "merge", "fix" (not before the blockers are fixed) or "discuss"; "" when it gave none.
	Verdict     string         `json:"verdict,omitempty"`
	VerdictNote string         `json:"verdictNote,omitempty"`
	Manual      []string       `json:"manual,omitempty"` // what to check by hand
	Comments    []DraftComment `json:"comments,omitempty"`
	Cost        float64        `json:"cost,omitempty"`
	Tokens      int            `json:"tokens,omitempty"`
}

// The verdicts of a ReviewDraft.
const (
	VerdictMerge   = "merge"
	VerdictFix     = "fix"
	VerdictDiscuss = "discuss"
)

// DraftComment is one of the reviewer's remarks as a comment to send to the MR from me: I edit it, strike it
// out, or send it.
type DraftComment struct {
	ID     string `json:"id"`     // "R1"
	Weight string `json:"weight"` // Must or Should
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Text   string `json:"text"`
	// State: "draft"; "dropped" (struck out); "sent" (URL is the comment); "failed" (Error says why).
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

// The states of a DraftComment.
const (
	DraftOpen    = "draft"
	DraftDropped = "dropped"
	DraftSent    = "sent"
	DraftFailed  = "failed"
)

// Clone: a copy to change: the store hands out shallow copies of a request, so its draft is never changed in place.
func (d *ReviewDraft) Clone() *ReviewDraft {
	if d == nil {
		return &ReviewDraft{}
	}
	c := *d
	c.Manual = append([]string(nil), d.Manual...)
	c.Comments = append([]DraftComment(nil), d.Comments...)
	return &c
}

// Spend is what one run of an agent for no task cost: the reviewer over someone else's MR (PhaseForMe).
type Spend struct {
	At      string  `json:"at"` // RFC 3339
	Phase   string  `json:"phase"`
	Project string  `json:"project,omitempty"`
	USD     float64 `json:"usd,omitempty"`
	Usage   Usage   `json:"usage"`
}

// AddSpends adds to the statistics' phases what the runs of no task since (zero: all of them) cost; project:
// of that project only ("" all). Taken, Cost and the rest stay the tasks'.
func AddSpends(st *Stats, spends []Spend, since time.Time, project string) {
	for _, s := range spends {
		if project != "" && s.Project != project {
			continue
		}
		if !since.IsZero() {
			at, err := time.Parse(time.RFC3339Nano, s.At)
			if err != nil || at.Before(since) {
				continue
			}
		}
		if st.Phases == nil {
			st.Phases = map[string]float64{}
		}
		if st.PhaseTokens == nil {
			st.PhaseTokens = map[string]int{}
		}
		if s.USD > 0 {
			st.Phases[s.Phase] += s.USD
		}
		if n := s.Usage.Total(); n > 0 {
			st.PhaseTokens[s.Phase] += n
		}
	}
}
