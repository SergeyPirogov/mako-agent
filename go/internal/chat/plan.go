package chat

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PlanStep: one numbered step of the agent's plan, as the board draws it: what it does, the rest of its text and
// the files it names.
type PlanStep struct {
	N     int      `json:"n"`
	Title string   `json:"title"`
	Body  string   `json:"body,omitempty"`
	Files []string `json:"files,omitempty"`
}

// PlanDoc: the agent's plan (ExitPlanMode) split into steps; Steps is empty when the plan has no numbered ones and
// the board draws it as it is. Intro: what comes before the steps; Risk: a section on the risks.
type PlanDoc struct {
	Intro string     `json:"intro,omitempty"`
	Steps []PlanStep `json:"steps"`
	Risk  string     `json:"risk,omitempty"`
	Files int        `json:"files,omitempty"` // how many files the steps name
}

var (
	planItem    = regexp.MustCompile(`^(\d+)[.)]\s+(.*)$`)                                           // 1. Do it
	planHeading = regexp.MustCompile(`^#{1,6}\s+(?:(?:Step|Крок)\s+)?(\d+)[.):]?\s*[-—:.]?\s*(.*)$`) // ### 1. Do it, ## Step 2: Do it; cyrillic-ok: plans in Ukrainian
	anyHeading  = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
	riskHeading = regexp.MustCompile(`(?i)^\**(risks?|ризик\S*)\**\s*:?\**\s*(.*)$`) // cyrillic-ok: plans in Ukrainian
	inlineCode  = regexp.MustCompile("`([^`\n]+)`")
	fileLike    = regexp.MustCompile(`^[\w.\-]+(/[\w.\-]+)*\.[A-Za-z0-9]{1,8}$|^[\w.\-]+(/[\w.\-]+)+/?$`)
)

// ParsePlan splits the agent's markdown plan into its steps: the items of a numbered list at the start of a line,
// or headings that number them («### 1. …», «## Step 2: …»). A «Risk» heading or line ends the steps and is kept
// apart. Steps in a nested list stay in their step's text.
func ParsePlan(md string) PlanDoc {
	doc := PlanDoc{Steps: []PlanStep{}}
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	// Headings that number steps win over a list: the list is then the steps' own text.
	byHeading := false
	for _, l := range lines {
		if planHeading.MatchString(strings.TrimSpace(l)) {
			byHeading = true
			break
		}
	}
	var intro, risk, body []string
	lead := "" // the rest of a step's first line, after its bold title
	var cur *PlanStep
	inRisk, fence := false, false
	flush := func() {
		if cur == nil {
			return
		}
		cur.Body = strings.TrimSpace(lead + "\n" + dedent(body))
		doc.Steps = append(doc.Steps, *cur)
		cur, body, lead = nil, nil, ""
	}
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			fence = !fence
		}
		if !fence {
			start := ""
			if byHeading {
				if m := planHeading.FindStringSubmatch(t); m != nil {
					start = m[1] + "\x00" + m[2]
				}
			} else if m := planItem.FindStringSubmatch(t); m != nil && l == strings.TrimLeft(l, " \t") {
				start = m[1] + "\x00" + m[2] // a nested item (indented) stays in its step's text
			}
			heading := anyHeading.FindStringSubmatch(t)
			label := t
			if heading != nil {
				label = heading[1]
			}
			if r := riskHeading.FindStringSubmatch(label); r != nil && (heading != nil || strings.HasPrefix(t, "**")) {
				flush()
				inRisk = true
				if r[2] != "" {
					risk = append(risk, r[2])
				}
				continue
			}
			if start != "" {
				flush()
				inRisk = false
				p := strings.SplitN(start, "\x00", 2)
				n, _ := strconv.Atoi(p[0])
				title, rest := splitTitle(p[1])
				cur = &PlanStep{N: n, Title: title}
				lead = rest
				continue
			}
			if heading != nil && cur != nil && !byHeading {
				// Another section after the list: it is not a step's text.
				flush()
			}
		}
		switch {
		case inRisk:
			risk = append(risk, l)
		case cur != nil:
			body = append(body, l)
		case len(doc.Steps) == 0:
			intro = append(intro, l)
		}
	}
	flush()
	doc.Intro, doc.Risk = strings.TrimSpace(strings.Join(intro, "\n")), strings.TrimSpace(strings.Join(risk, "\n"))
	seen := map[string]bool{}
	for i := range doc.Steps {
		s := &doc.Steps[i]
		// Numbers as written may skip or repeat: the board counts them.
		s.N = i + 1
		for _, m := range inlineCode.FindAllStringSubmatch(s.Title+"\n"+s.Body, -1) {
			f := strings.TrimSpace(m[1])
			if fileLike.MatchString(f) && !contains(s.Files, f) {
				s.Files = append(s.Files, f)
				seen[f] = true
			}
		}
	}
	doc.Files = len(seen)
	if len(doc.Steps) == 0 {
		doc.Intro, doc.Risk = "", ""
	}
	return doc
}

// splitTitle: a step's first line without the bold around it. «**Do it** — how» is the title «Do it» and «how»,
// which goes to the step's text.
func splitTitle(s string) (title, rest string) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "**") {
		if end := strings.Index(s[2:], "**"); end > 0 {
			rest = strings.TrimSpace(strings.TrimLeft(s[end+4:], " :—–-"))
			s = strings.TrimSpace(s[2 : end+2])
		}
	}
	return strings.TrimRight(s, ":"), rest
}

// dedent drops the indent a step's text shares, so its nested list is read as a list.
func dedent(lines []string) string {
	min := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if min < 0 || n < min {
			min = n
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if len(l) >= min && min > 0 {
			l = l[min:]
		}
		out[i] = l
	}
	return strings.Join(out, "\n")
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// PlanNotes: what you wrote on the agent's plan: notes on its steps and one on the whole of it.
type PlanNotes struct {
	Steps []StepNote `json:"steps"`
	Whole string     `json:"whole"`
}

// StepNote: a note on step N (Title: the step, so the agent knows which one is meant).
type StepNote struct {
	N     int    `json:"n"`
	Title string `json:"title"`
	Note  string `json:"note"`
}

// Text: the notes as the agent reads them, each on its step's number.
func (p PlanNotes) Text() string {
	var out []string
	for _, s := range p.Steps {
		note := strings.TrimSpace(s.Note)
		if note == "" {
			continue
		}
		if t := strings.TrimSpace(s.Title); t != "" {
			out = append(out, fmt.Sprintf("Step %d (%s): %s", s.N, t, note))
		} else {
			out = append(out, fmt.Sprintf("Step %d: %s", s.N, note))
		}
	}
	text := strings.Join(out, "\n")
	if w := strings.TrimSpace(p.Whole); w != "" {
		if text != "" {
			text += "\n\nOn the whole plan: " + w
		} else {
			text = w
		}
	}
	return text
}
