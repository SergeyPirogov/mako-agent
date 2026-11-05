package core

import (
	"fmt"
	"testing"
)

// past: an ended task of project g/app the estimate called size, touching parts; cost and minutes to its MR.
func past(n int, size, parts string, rec Record, cost float64, minutes int) Record {
	rec.ID = fmt.Sprintf("g/app#%d", n)
	rec.Task = Task{ID: rec.ID, Project: "g/app", Title: fmt.Sprintf("task %d", n)}
	rec.Estimate = &Estimate{State: "done", Size: size, Parts: parts}
	rec.StartedAt = fmt.Sprintf("2026-10-%02dT10:00:00Z", n%28+1)
	rec.FinishedAt = fmt.Sprintf("2026-10-%02dT10:%02d:00Z", n%28+1, minutes)
	rec.Cost = cost
	return rec
}

var (
	pastMerged = Record{Status: Done, ReviewState: "merged"}
	pastRedone = Record{Status: Done, ReviewState: "merged", Attempt: 2}
	pastFailed = Record{Status: Failed, Error: "tests failed"}
)

// fresh: the new task, estimated so.
func fresh(size, parts string) Record {
	return Record{ID: "g/app#100", Status: Asking, Task: Task{ID: "g/app#100", Project: "g/app", Title: "new one"},
		Estimate: &Estimate{State: "done", Size: size, Parts: parts}}
}

func TestOutlookNoneWithoutHistory(t *testing.T) {
	if o := OutlookOf(nil, fresh("small", ""), ""); o != nil {
		t.Fatalf("empty history: %+v", o)
	}
	var four []Record
	for i := 1; i <= OutlookMin-1; i++ {
		four = append(four, past(i, "small", "", pastMerged, 1, 10))
	}
	if o := OutlookOf(four, fresh("small", ""), ""); o != nil {
		t.Fatalf("%d tasks: %+v", len(four), o)
	}
	// Ones that do not count: another project, another size, not ended, stopped on purpose, no estimate.
	other := past(10, "small", "", pastMerged, 1, 10)
	other.Task.Project = "g/other"
	running := past(11, "small", "", Record{Status: HandedOff}, 1, 10)
	review := past(12, "small", "", Record{Status: Done}, 1, 10)
	stopped := past(13, "small", "", Record{Status: Cancelled, Error: "Cancelled by the developer"}, 1, 10)
	bare := past(14, "small", "", pastMerged, 1, 10)
	bare.Estimate = nil
	all := append(four, other, past(15, "big", "", pastMerged, 1, 10), running, review, stopped, bare)
	if o := OutlookOf(all, fresh("small", ""), ""); o != nil {
		t.Fatalf("only %d count: %+v", len(four), o)
	}
	// The new task without an estimate of its size has none either.
	enough := append(four, past(5, "small", "", pastMerged, 1, 10))
	if o := OutlookOf(enough, Record{ID: "g/app#100", Task: Task{Project: "g/app"}}, ""); o != nil {
		t.Fatalf("not estimated: %+v", o)
	}
	if o := OutlookOf(enough, fresh("small", ""), ""); o == nil || o.Tasks != OutlookMin {
		t.Fatalf("five: %+v", o)
	}
}

func TestOutlookNumbers(t *testing.T) {
	history := []Record{
		past(1, "medium", "", pastMerged, 1, 10),
		past(2, "medium", "", pastMerged, 2, 20),
		past(3, "medium", "", pastRedone, 3, 30),
		past(4, "medium", "", pastMerged, 4, 40),
		past(5, "medium", "", pastFailed, 5, 0),
		past(6, "medium", "", Record{Status: Done, ReviewState: "closed", Error: "MR closed"}, 0, 0),
	}
	o := OutlookOf(history, fresh("medium", ""), "")
	if o == nil {
		t.Fatal("no outlook")
	}
	if o.Tasks != 6 || o.BySimilar || o.Size != "medium" {
		t.Fatalf("outlook %+v", o)
	}
	// Costs 1..5 (the sixth not priced): median 3, p80 the fourth of five, 4.
	if o.Priced != 5 || !near(o.Cost, 3) || !near(o.Cost80, 4) {
		t.Errorf("cost %v p80 %v of %d", o.Cost, o.Cost80, o.Priced)
	}
	// Times of the tasks with an MR (status done, the closed MR's too has no time: its FinishedAt is not after).
	if o.Timed != 4 || !near(o.Seconds, 25*60) || !near(o.Seconds80, 40*60) {
		t.Errorf("time %v p80 %v of %d", o.Seconds, o.Seconds80, o.Timed)
	}
	if !near(o.FirstPass, 3.0/6) || !near(o.FailRate, 2.0/6) {
		t.Errorf("first pass %v, failed %v", o.FirstPass, o.FailRate)
	}
	if len(o.Reasons) != 2 {
		t.Errorf("reasons %+v", o.Reasons)
	}
	if len(o.Similar) != 3 {
		t.Errorf("similar %+v", o.Similar)
	}
}

// With enough tasks touching the same parts of the code only they count, and the most alike come first with
// what their retro found; with too few, all the tasks of the size do.
func TestOutlookBySimilarParts(t *testing.T) {
	var history []Record
	for i := 1; i <= 5; i++ {
		history = append(history, past(i, "small", "internal/web/board.html, style.css", pastMerged, 1, 10))
	}
	for i := 6; i <= 10; i++ {
		history = append(history, past(i, "small", "internal/trackers/jira.go", pastFailed, 9, 0))
	}
	best := past(11, "small", "internal/web/board.html", pastRedone, 2, 20)
	best.Retro = &Retro{State: "done", Bad: "\n- the theme broke on the phone\n- another", Next: "- check both themes"}
	history = append(history, best)
	o := OutlookOf(history, fresh("small", "internal/web/board.html"), "")
	if o == nil || !o.BySimilar || o.Tasks != 6 {
		t.Fatalf("by similar: %+v", o)
	}
	if !near(o.FirstPass, 5.0/6) || o.FailRate != 0 {
		t.Errorf("first pass %v, failed %v", o.FirstPass, o.FailRate)
	}
	if len(o.Similar) != 3 || o.Similar[0].ID != best.ID || o.Similar[0].Retro != "the theme broke on the phone" || o.Similar[0].FirstPass || !o.Similar[0].Merged {
		t.Errorf("similar %+v", o.Similar)
	}
	// Few alike: the size's tasks all count.
	o = OutlookOf(history, fresh("small", "internal/trackers/gitlab.go docs"), "")
	if o == nil || o.BySimilar || o.Tasks != 11 {
		t.Fatalf("by size: %+v", o)
	}
}

func TestOutlookModelAdvice(t *testing.T) {
	model := func(rec Record, m string) Record { rec.AgentModel = m; return rec }
	var history []Record
	n := 0
	add := func(rec Record, m string, cost float64) {
		n++
		history = append(history, model(past(n, "medium", "", rec, cost, 10), m))
	}
	// opus: 1 of 4 first pass at $2; sonnet: 3 of 4 at $2.20.
	add(pastMerged, "claude-opus-4-7", 2)
	add(pastRedone, "opus", 2)
	add(pastFailed, "opus", 2)
	add(pastRedone, "opus", 2)
	for range 3 {
		add(pastMerged, "claude-sonnet-4-6", 2.2)
	}
	add(pastFailed, "sonnet", 2.2)
	o := OutlookOf(history, fresh("medium", ""), "opus")
	if o == nil || o.Advice == nil {
		t.Fatalf("no advice: %+v", o)
	}
	if a := o.Advice; a.Model != "sonnet" || a.Than != "opus" || !near(a.FirstPass, 0.75) || !near(a.ThanFirstPass, 0.25) || a.Tasks != 4 {
		t.Errorf("advice %+v", a)
	}
	// Starting with sonnet already: nothing better to advise.
	if o := OutlookOf(history, fresh("medium", ""), "sonnet"); o.Advice != nil {
		t.Errorf("sonnet itself: %+v", o.Advice)
	}
	// Much dearer: not advised.
	for i := range history {
		if ModelFamily(history[i].AgentModel) == "sonnet" {
			history[i].Cost = 6
		}
	}
	if o := OutlookOf(history, fresh("medium", ""), "opus"); o.Advice != nil {
		t.Errorf("dear: %+v", o.Advice)
	}
	// A model with too few tasks is not weighed.
	few := append([]Record(nil), history[:4]...)
	few = append(few, model(past(20, "medium", "", pastMerged, 1, 10), "haiku"), model(past(21, "medium", "", pastMerged, 1, 10), "haiku"))
	if o := OutlookOf(few, fresh("medium", ""), "opus"); o == nil || o.Advice != nil || len(o.Models) != 1 {
		t.Errorf("few: %+v", o)
	}
}

func TestPercentile(t *testing.T) {
	if percentile(nil, 0.8) != 0 {
		t.Error("none")
	}
	if p := percentile([]float64{5, 1, 3, 2, 4}, 0.8); p != 4 {
		t.Errorf("p80 %v", p)
	}
	if p := percentile([]float64{7}, 0.8); p != 7 {
		t.Errorf("one %v", p)
	}
}
