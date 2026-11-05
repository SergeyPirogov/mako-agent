package core

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// The outlook: what the history says of a new task before it starts, with no model asked. The tasks of the same
// project that the estimate called the same size and that ended (merged, or failed short of a merge) say what a
// task like it costs, how long it takes to its MR, how often it goes through with no rework and why the others
// failed. With enough of them touching the same parts of the code (Estimate.Parts), only those count.

// OutlookMin: the fewest ended tasks an outlook is made of; fewer, and there is none.
const OutlookMin = 5

// outlookModelMin: the fewest ended tasks of one model for the outlook to weigh it against another.
const outlookModelMin = 3

// Outlook is how the ended tasks like a new one went.
type Outlook struct {
	// Tasks: the ended tasks it is made of; BySimilar: only those touching the same parts of the code.
	Tasks     int    `json:"tasks"`
	BySimilar bool   `json:"bySimilar,omitempty"`
	Size      string `json:"size"`
	// Cost, Cost80: the median and the 80th percentile of USD Mako's agent spent, over the Priced tasks.
	Cost   float64 `json:"cost"`
	Cost80 float64 `json:"cost80"`
	Priced int     `json:"priced"`
	// Seconds, Seconds80: the same of the time from the hand-off to the MR, over the Timed tasks.
	Seconds   float64 `json:"seconds"`
	Seconds80 float64 `json:"seconds80"`
	Timed     int     `json:"timed"`
	// FirstPass: the share of Tasks merged with no rework (another attempt, review fixes); FailRate: of failed.
	FirstPass float64 `json:"firstPass"`
	FailRate  float64 `json:"failRate"`
	// Reasons: why the failed ones failed, the most frequent first (three at most).
	Reasons []Reason `json:"reasons,omitempty"`
	// Similar: the ended tasks most like this one, the most alike first (three at most).
	Similar []SimilarTask `json:"similar,omitempty"`
	// Models: how the tasks of each model went, the ones with enough tasks; Advice: another model did better.
	Models []OutlookModel `json:"models,omitempty"`
	Advice *ModelAdvice   `json:"advice,omitempty"`
}

// SimilarTask is one ended task like the new one, with what its retro found.
type SimilarTask struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Merged    bool    `json:"merged"`
	FirstPass bool    `json:"firstPass"`
	Cost      float64 `json:"cost,omitempty"`
	Seconds   float64 `json:"seconds,omitempty"`
	// Retro: the first point of what went wrong in its retro, else of what to do next time; "" without one.
	Retro string `json:"retro,omitempty"`
}

// OutlookModel is how the outlook's tasks of one model went.
type OutlookModel struct {
	Model     string  `json:"model"`
	Tasks     int     `json:"tasks"`
	FirstPass float64 `json:"firstPass"`
	Cost      float64 `json:"cost"` // the median, 0 when none is priced
}

// ModelAdvice: Model went through with no rework more often than Than, the model the task would start with (or
// all the tasks when it has too few), at a close price.
type ModelAdvice struct {
	Model         string  `json:"model"`
	Tasks         int     `json:"tasks"`
	FirstPass     float64 `json:"firstPass"`
	Cost          float64 `json:"cost"`
	Than          string  `json:"than,omitempty"` // "" all the tasks of the outlook
	ThanFirstPass float64 `json:"thanFirstPass"`
	ThanCost      float64 `json:"thanCost"`
}

// adviceGap: how much more often another model must go through with no rework to be advised; advicePrice: how
// much dearer it may be.
const adviceGap, advicePrice = 0.15, 1.25

// ended: r merged, or failed short of a merge; a task stopped on purpose says nothing of how it would have gone.
func ended(r Record) (merged, ok bool) {
	merged = r.Status == Done && r.ReviewState == "merged"
	failed := r.Status == Failed || r.Status == Cancelled || r.ReviewState == "closed"
	return merged, merged || failed && !cancelled(r)
}

// similarMin: how alike (Jaccard of their Parts' words) two tasks must be to count as touching the same parts of
// the code; a single shared word among many does not.
const similarMin = 0.2

// History is the ended tasks the outlooks are made of, indexed once so that many new tasks are weighed over one
// pass of the records: the words of each one's Parts are cut once, not per new task.
type History struct {
	by map[histKey][]endedTask
}

type histKey struct{ project, size string }

// endedTask: an ended task with an estimate of its size, with the words of its Parts.
type endedTask struct {
	r      Record
	merged bool
	words  map[string]bool
}

// NewHistory indexes the records that ended (merged, or failed short of a merge) and have an estimate of their size.
func NewHistory(records []Record) *History {
	h := &History{by: map[histKey][]endedTask{}}
	for _, x := range records {
		if x.Estimate == nil || x.Estimate.State != "done" || x.Estimate.Size == "" {
			continue
		}
		merged, ok := ended(x)
		if !ok {
			continue
		}
		k := histKey{projectOf(x), x.Estimate.Size}
		h.by[k] = append(h.by[k], endedTask{r: x, merged: merged, words: wordsOf(x.Estimate.Parts)})
	}
	return h
}

// OutlookOf: the outlook of r by the other records (see History.Outlook); one call indexes them all, so for
// several tasks use NewHistory once.
func OutlookOf(records []Record, r Record, startModel string) *Outlook {
	return NewHistory(records).Outlook(r, startModel)
}

// Outlook: the outlook of r, nil when r has no estimate of its size yet or the history is too short. startModel:
// the model r's agent would start with, the one the advice weighs another against.
func (h *History) Outlook(r Record, startModel string) *Outlook {
	e := r.Estimate
	if e == nil || e.State != "done" || e.Size == "" {
		return nil
	}
	words := wordsOf(e.Parts)
	type scored struct {
		endedTask
		alike float64
	}
	var all, similar []scored
	for _, x := range h.by[histKey{projectOf(r), e.Size}] {
		if x.r.ID == r.ID {
			continue
		}
		p := scored{endedTask: x, alike: jaccard(words, x.words)}
		all = append(all, p)
		if p.alike >= similarMin {
			similar = append(similar, p)
		}
	}
	o := &Outlook{Size: e.Size}
	pick := all
	if len(similar) >= OutlookMin {
		pick, o.BySimilar = similar, true
	}
	if len(pick) < OutlookMin {
		return nil
	}
	o.Tasks = len(pick)
	var costs, secs []float64
	reasons := map[string]*Reason{}
	type modelAcc struct {
		tasks, clean int
		costs        []float64
	}
	models := map[string]*modelAcc{}
	cleanN, failed := 0, 0
	for _, p := range pick {
		x := p.r
		first := p.merged && clean(x)
		if first {
			cleanN++
		}
		if !p.merged {
			failed++
			key, text := reasonOf(x)
			if reasons[key] == nil {
				reasons[key] = &Reason{Key: key, Text: text}
			}
			reasons[key].Count++
		}
		if x.Cost > 0 {
			costs = append(costs, x.Cost)
		}
		if d, ok := between(x.StartedAt, x.FinishedAt); ok && x.Status == Done {
			secs = append(secs, d.Seconds())
		}
		m := ModelFamily(x.AgentModel)
		if models[m] == nil {
			models[m] = &modelAcc{}
		}
		models[m].tasks++
		if first {
			models[m].clean++
		}
		if x.Cost > 0 {
			models[m].costs = append(models[m].costs, x.Cost)
		}
	}
	o.Priced, o.Timed = len(costs), len(secs)
	o.Cost, o.Cost80 = median(costs), percentile(costs, 0.8)
	o.Seconds, o.Seconds80 = median(secs), percentile(secs, 0.8)
	o.FirstPass = float64(cleanN) / float64(o.Tasks)
	o.FailRate = float64(failed) / float64(o.Tasks)
	for _, x := range reasons {
		o.Reasons = append(o.Reasons, *x)
	}
	sort.Slice(o.Reasons, func(i, j int) bool {
		a, b := o.Reasons[i], o.Reasons[j]
		return a.Count > b.Count || a.Count == b.Count && a.Text < b.Text
	})
	o.Reasons = o.Reasons[:min(len(o.Reasons), 3)]
	for name, m := range models {
		if name == "—" || m.tasks < outlookModelMin {
			continue
		}
		o.Models = append(o.Models, OutlookModel{Model: name, Tasks: m.tasks, FirstPass: float64(m.clean) / float64(m.tasks), Cost: median(m.costs)})
	}
	sort.Slice(o.Models, func(i, j int) bool {
		a, b := o.Models[i], o.Models[j]
		return a.FirstPass > b.FirstPass || a.FirstPass == b.FirstPass && a.Model < b.Model
	})
	o.Advice = advise(o, ModelFamily(startModel))
	// The most alike first, the latest of the equally alike.
	sort.SliceStable(pick, func(i, j int) bool {
		if pick[i].alike != pick[j].alike {
			return pick[i].alike > pick[j].alike
		}
		return pick[i].r.StartedAt > pick[j].r.StartedAt
	})
	for _, p := range pick[:min(len(pick), 3)] {
		x := p.r
		s := SimilarTask{ID: x.ID, Title: x.Task.Title, Merged: p.merged, FirstPass: p.merged && clean(x), Cost: x.Cost}
		if d, ok := between(x.StartedAt, x.FinishedAt); ok && x.Status == Done {
			s.Seconds = d.Seconds()
		}
		if rt := x.Retro; rt != nil && rt.State == "done" {
			if s.Retro = firstPoint(rt.Bad); s.Retro == "" {
				s.Retro = firstPoint(rt.Next)
			}
		}
		o.Similar = append(o.Similar, s)
	}
	return o
}

// advise: the model of the outlook that went through with no rework clearly more often than start (or than all
// its tasks, when start has too few) at a close price; nil when none did.
func advise(o *Outlook, start string) *ModelAdvice {
	than, thanPass, thanCost := "", o.FirstPass, o.Cost
	for _, m := range o.Models {
		if m.Model == start {
			than, thanPass, thanCost = m.Model, m.FirstPass, m.Cost
		}
	}
	for _, m := range o.Models { // the best first
		if m.Model == start || m.Model == than {
			continue
		}
		if m.FirstPass < thanPass+adviceGap {
			return nil
		}
		if thanCost > 0 && m.Cost > thanCost*advicePrice {
			continue
		}
		return &ModelAdvice{Model: m.Model, Tasks: m.Tasks, FirstPass: m.FirstPass, Cost: m.Cost, Than: than, ThanFirstPass: thanPass, ThanCost: thanCost}
	}
	return nil
}

// percentile: the value p (0..1) of the way up the sorted xs, by the nearest rank; 0 for none.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

// stopWords: the words every task's text has, that make no two tasks alike.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "into": true, "that": true, "this": true,
	"add": true, "fix": true, "make": true, "use": true, "new": true, "not": true, "its": true, "are": true,
	"для": true, "або": true, "щоб": true, "якщо": true, "коли": true, "також": true, "без": true, "після": true, // cyrillic-ok: the titles' Ukrainian words
}

// wordsOf: the words and paths of a text (lowercased, three letters or more, no stop words) as a set.
func wordsOf(text string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '/' && r != '.' && r != '_'
	}) {
		w = strings.Trim(w, "./_")
		if len([]rune(w)) >= 3 && !stopWords[w] {
			set[w] = true
		}
	}
	return set
}

// jaccard: how alike two sets of words are, 0 (nothing in common) to 1 (the same).
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	both := 0
	for w := range a {
		if b[w] {
			both++
		}
	}
	return float64(both) / float64(len(a)+len(b)-both)
}

// firstPoint: the first point of Markdown bullets, without its bullet; "" when none.
func firstPoint(md string) string {
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimLeft(line, "-*•"))
		if line != "" {
			if r := []rune(line); len(r) > 200 {
				line = string(r[:200]) + "…"
			}
			return line
		}
	}
	return ""
}
