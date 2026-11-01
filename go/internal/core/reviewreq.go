package core

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
}
