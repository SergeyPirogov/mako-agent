package core

// Spec is an idea written on the board («New idea»), on its way to tasks: the agent reads the project's code and
// writes a specification, asks what it cannot tell (answered on the board), and proposes the tasks to do it in.
// Nothing reaches the tracker until you press «Create in tracker»: then a parent issue with the specification and
// its subtasks (as a split, Breakdown) are opened. Kept in specs.json, not in the state: it is no task yet.
type Spec struct {
	ID      string `json:"id"`
	Tracker string `json:"tracker"`
	Project string `json:"project"`
	// Text: the idea as written; Images: the pictures attached to it, on this Mac.
	Text   string   `json:"text"`
	Images []string `json:"images,omitempty"`
	// Pictures: the Markdown of the images once uploaded to the tracker, for the parent issue.
	Pictures []string `json:"pictures,omitempty"`
	At       string   `json:"at"`
	// State: "running" (the agent writes), "questions" (it waits for your answers), "ready" (the spec and its
	// tasks to edit), "failed", "creating" or "created".
	State string `json:"state"`
	// RunAt: when the agent's last run started (a run that never ended, Mako having stopped, is stale).
	RunAt string `json:"runAt,omitempty"`
	Error string `json:"error,omitempty"`
	// Title and Body: the parent issue's title and the specification, in Markdown.
	Title     string         `json:"title,omitempty"`
	Body      string         `json:"body,omitempty"`
	Questions []SpecQuestion `json:"questions,omitempty"`
	// Answered: the questions were answered; the agent asks no more.
	Answered bool        `json:"answered,omitempty"`
	Items    []SplitItem `json:"items,omitempty"`
	// Parent: the parent issue once it is created (none when the idea is one task: Items[0].Task is it).
	Parent  string  `json:"parent,omitempty"`
	Created string  `json:"created,omitempty"`
	Cost    float64 `json:"cost,omitempty"`
}

// SpecQuestion is the agent's question about an idea: what it assumes without an answer, and your answer.
type SpecQuestion struct {
	Q      string `json:"q"`
	Assume string `json:"assume,omitempty"`
	A      string `json:"a,omitempty"`
}

// SpecDraft is what the agent wrote for an idea: the specification and either its questions or its tasks.
type SpecDraft struct {
	Title, Body string
	Questions   []SpecQuestion
	Items       []SplitItem
}
