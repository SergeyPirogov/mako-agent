package writer

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// The reviewer's remarks and the agent's report on them keep to a fixed format, one line per remark, so that
// Mako can tell which is which: «R2 [треба] pay.go:42 — …» from the reviewer, «R2 — виправлено …» from the agent,
// «R2: закрито» from the next round. Markdown around it (a list's dash, bold, backticks) does not matter.

var (
	// remarkLine: «R3 [must] path:42 — text»; the weight in square or round brackets.
	remarkLine = regexp.MustCompile(`^\s*(?:[-*•+]|\d+[.)])?\s*[*_]*\s*R(\d+)\s*[*_]*\s*[\[(]\s*([^\])]+?)\s*[\])]\s*[*_]*\s*(.*)$`)
	// idLine: «R3 — text» or «R3: text», a verdict on a remark or the agent's answer on one.
	idLine = regexp.MustCompile(`^\s*(?:[-*•+]|\d+[.)])?\s*[*_]*\s*R(\d+)\s*[*_]*\s*(?:—|–|-|:)\s*[*_]*\s*(.+)$`)
	// place: the file (and line) a remark opens with, «pay.go:42», «`pay.go:42-50`» or «pay.go».
	place = regexp.MustCompile("^`?([^\\s`—–:]+\\.[^\\s`—–:]+|[^\\s`—–:]+/[^\\s`—–:]+)(?::(\\d+)(?:[-–]\\d+)?)?`?\\s*(?:—|–|-|:)\\s*(.+)$")
	lead  = regexp.MustCompile(`^\s*(?:—|–|-|:)\s*`)
)

// weightOf: the remark's weight from the reviewer's word for it, in either language.
func weightOf(w string) (string, bool) {
	switch strings.ToLower(strings.Trim(w, " *_`")) {
	case "must", "треба", "обов'язково", "обовʼязково", "required", "blocker":
		return core.Must, true
	case "should", "варто", "бажано", "nice", "optional", "minor":
		return core.Should, true
	}
	return "", false
}

// ParseRemarks: the reviewer's numbered remarks in its answer, in their order; lines indented under one go on
// with its text. None when the answer does not keep to the format.
func ParseRemarks(text string) []core.Remark {
	var out []core.Remark
	last := -1
	for _, l := range strings.Split(text, "\n") {
		if m := remarkLine.FindStringSubmatch(l); m != nil {
			w, ok := weightOf(m[2])
			if !ok {
				last = -1
				continue
			}
			r := core.Remark{ID: "R" + m[1], Weight: w, Text: strings.TrimSpace(m[3])}
			if p := place.FindStringSubmatch(r.Text); p != nil {
				r.File, r.Text = p[1], strings.TrimSpace(p[3])
				r.Line, _ = strconv.Atoi(p[2])
			} else {
				r.Text = lead.ReplaceAllString(r.Text, "")
			}
			r.Text = strings.TrimSpace(r.Text)
			out = append(out, r)
			last = len(out) - 1
			continue
		}
		if last >= 0 && strings.TrimSpace(l) != "" && (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && idLine.FindStringSubmatch(l) == nil {
			out[last].Text += "\n" + strings.TrimSpace(l)
			continue
		}
		last = -1
	}
	return out
}

// verdictOf: the next round's verdict on an earlier remark, and what it adds.
func verdictOf(s string) (string, string) {
	low := strings.ToLower(s)
	for _, v := range []struct{ verdict, word string }{
		{core.Open, "не закрито"}, {core.Open, "not closed"}, {core.Open, "відкрито"}, {core.Open, "open"},
		{core.Accepted, "приймаю пояснення"}, {core.Accepted, "приймаю"}, {core.Accepted, "explanation accepted"}, {core.Accepted, "accept the explanation"}, {core.Accepted, "accepted"},
		{core.Closed, "закрито"}, {core.Closed, "closed"},
	} {
		if strings.HasPrefix(low, v.word) {
			return v.verdict, note(s[len(v.word):])
		}
	}
	return "", ""
}

// replyOf: the agent's answer on a remark, and what it adds (how, or why not).
func replyOf(s string) (string, string) {
	low := strings.ToLower(s)
	for _, v := range []struct{ reply, word string }{
		{core.Partly, "частково виправлено"}, {core.Partly, "частково"}, {core.Partly, "partially fixed"}, {core.Partly, "partially"}, {core.Partly, "partly"},
		{core.NotFixed, "не виправлено"}, {core.NotFixed, "не виправив"}, {core.NotFixed, "not fixed"}, {core.NotFixed, "won't fix"},
		{core.Fixed, "виправлено"}, {core.Fixed, "виправив"}, {core.Fixed, "fixed"}, {core.Fixed, "done"},
	} {
		if strings.HasPrefix(low, v.word) {
			return v.reply, note(s[len(v.word):])
		}
	}
	return "", ""
}

// note: what follows the verdict's word, without the punctuation that joins them.
func note(s string) string {
	s = strings.TrimSpace(strings.TrimLeft(s, " *_:;,.—–-"))
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return clipTo(s, 600)
}

// ParseChecks: the verdicts of a round on the earlier remarks known (by ID); unknown IDs and lines with no
// verdict are not checks.
func ParseChecks(text string, known map[string]bool) []core.Check {
	var out []core.Check
	got := map[string]bool{}
	for _, l := range strings.Split(text, "\n") {
		if remarkLine.MatchString(l) {
			continue
		}
		m := idLine.FindStringSubmatch(l)
		if m == nil || !known["R"+m[1]] || got["R"+m[1]] {
			continue
		}
		if v, n := verdictOf(strings.TrimSpace(m[2])); v != "" {
			out = append(out, core.Check{ID: "R" + m[1], Verdict: v, Note: n})
			got["R"+m[1]] = true
		}
	}
	return out
}

// A Reply is the agent's answer on one remark.
type Reply struct{ Reply, Note string }

// ParseReport: the agent's answers on the remarks (by ID) in its report; empty when it wrote none.
func ParseReport(text string, ids []string) map[string]Reply {
	known := map[string]bool{}
	for _, id := range ids {
		known[id] = true
	}
	out := map[string]Reply{}
	for _, l := range strings.Split(text, "\n") {
		m := idLine.FindStringSubmatch(l)
		if m == nil || !known["R"+m[1]] {
			continue
		}
		if r, n := replyOf(strings.TrimSpace(m[2])); r != "" {
			out["R"+m[1]] = Reply{r, n}
		}
	}
	return out
}

// Clean: a round over remarks is clean when none of them, new or carried over still open, is a «must».
func Clean(remarks []core.Remark) bool {
	for _, r := range remarks {
		if r.Weight == core.Must {
			return false
		}
	}
	return true
}

// Numbered gives the round's new remarks their IDs, in order from next: they go on across the task's rounds,
// whatever numbers the reviewer wrote.
func Numbered(remarks []core.Remark, next int) []core.Remark {
	out := append([]core.Remark(nil), remarks...)
	for i := range out {
		out[i].ID = "R" + strconv.Itoa(next+i)
	}
	return out
}

// weightWord: the weight as people read it.
func weightWord(w string) string {
	if w == core.Must {
		return i18n.TA("review.must")
	}
	return i18n.TA("review.should")
}

// RemarkLine: a remark in the reviewer's format, «R2 [треба] pay.go:42 — text».
func RemarkLine(r core.Remark) string {
	s := r.ID + " [" + weightWord(r.Weight) + "] "
	if r.File != "" {
		s += "`" + r.File
		if r.Line > 0 {
			s += ":" + strconv.Itoa(r.Line)
		}
		s += "` — "
	}
	return s + r.Text
}

// RemarksList: the remarks as a Markdown list, one per item.
func RemarksList(remarks []core.Remark) string {
	var b strings.Builder
	for _, r := range remarks {
		b.WriteString("- " + strings.ReplaceAll(RemarkLine(r), "\n", "\n  ") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// replyWord: the agent's answer as people read it.
func replyWord(r string) string {
	switch r {
	case core.Fixed:
		return "✅ " + i18n.TA("review.fixed")
	case core.NotFixed:
		return "❌ " + i18n.TA("review.notFixed")
	case core.Partly:
		return "◐ " + i18n.TA("review.partly")
	}
	return "⏳ " + i18n.TA("review.noReply")
}

// verdictWord: the next round's verdict as people read it.
func verdictWord(v string) string {
	switch v {
	case core.Closed:
		return "✅ " + i18n.TA("review.closed")
	case core.Accepted:
		return "✅ " + i18n.TA("review.accepted")
	case core.Open:
		return "❌ " + i18n.TA("review.open")
	}
	return "⏳"
}

// ReviewComment: the round as a comment for the tracker: its verdicts on the earlier remarks, then its own.
func ReviewComment(round int, a core.AgentReview) string {
	var b strings.Builder
	b.WriteString(i18n.TA("comment.review", round) + "\n")
	if len(a.Checks) > 0 {
		b.WriteString("\n" + i18n.TA("comment.reviewChecks") + "\n")
		for _, c := range a.Checks {
			line := "- " + c.ID + " — " + verdictWord(c.Verdict)
			if c.Note != "" {
				line += ": " + c.Note
			}
			b.WriteString(line + "\n")
		}
	}
	if len(a.Remarks) > 0 {
		b.WriteString("\n" + RemarksList(a.Remarks) + "\n")
	} else if a.State == "clean" {
		b.WriteString("\n" + i18n.TA("comment.reviewClean") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ReplyComment: the agent's report on the remarks, as a comment for the tracker; link is the reviewer's comment.
func ReplyComment(a core.AgentReview, link string) string {
	var b strings.Builder
	b.WriteString(i18n.TA("comment.reviewReply"))
	if link != "" {
		b.WriteString(" " + i18n.TA("comment.reviewReplyTo", link))
	}
	b.WriteString("\n\n")
	answered := false
	for _, r := range a.Remarks {
		line := "- " + r.ID + " — " + replyWord(r.Reply)
		if r.ReplyNote != "" {
			line += ": " + r.ReplyNote
		}
		answered = answered || r.Reply != ""
		b.WriteString(line + "\n")
	}
	if !answered {
		b.WriteString("\n" + i18n.TA("comment.reviewNoReport") + "\n")
	}
	if a.Unverified {
		b.WriteString("\n⚠️ " + i18n.TA("comment.reviewUnverified") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// PriorText: the earlier rounds' remarks with the agent's answers on them, for the next round to check.
func PriorText(remarks []core.Remark) string {
	var b strings.Builder
	for _, r := range remarks {
		b.WriteString("- " + strings.ReplaceAll(RemarkLine(r), "\n", "\n  ") + "\n")
		ans := "no report"
		switch r.Reply {
		case core.Fixed:
			ans = "fixed"
		case core.NotFixed:
			ans = "not fixed"
		case core.Partly:
			ans = "partly fixed"
		}
		if r.ReplyNote != "" {
			ans += ": " + r.ReplyNote
		}
		b.WriteString("  The agent's answer: " + ans + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
