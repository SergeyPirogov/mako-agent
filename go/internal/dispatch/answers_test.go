package dispatch

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
)

func TestSplitQuestions(t *testing.T) {
	got := SplitQuestions("- Яка саме кнопка?\n\n* Де вона\n  на сторінці?\n1. Який браузер?\n2) Чи є логи?\n")
	want := []string{"Яка саме кнопка?", "Де вона на сторінці?", "Який браузер?", "Чи є логи?"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("questions: %q", got)
	}
	if got := SplitQuestions("Одне питання без маркера"); !reflect.DeepEqual(got, []string{"Одне питання без маркера"}) {
		t.Fatalf("no marker: %q", got)
	}
	if got := SplitQuestions(" \n"); len(got) != 0 {
		t.Fatalf("empty: %q", got)
	}
}

func TestAnswersCommentOnlyFilled(t *testing.T) {
	qs := []string{"Яка кнопка?", "Де?", "Який браузер?"}
	got := AnswersComment(qs, map[string]string{"Який браузер?": "Safari", "Яка кнопка?": "«Оплатити»", "Де?": "  "})
	want := "**Відповіді на питання до задачі:**\n\n1. Яка кнопка? — «Оплатити»\n2. Який браузер? — Safari"
	if got != want {
		t.Fatalf("comment:\n%s", got)
	}
	if AnswersComment(qs, map[string]string{"Де?": " "}) != "" || AnswersComment(qs, nil) != "" {
		t.Fatal("a comment without answers")
	}
}

// newWithQuestions: the task of setup in «Нові», estimated with two questions.
func newWithQuestions(t *testing.T, d *Dispatcher) core.Record {
	d.Poll(context.Background())
	d.Store.Update("grp/app#5", func(r *core.Record) {
		r.Estimate = &core.Estimate{State: "done", Size: "small", For: EstimateKey(r.Task), Questions: "- Яка саме кнопка?\n- Який браузер?"}
	})
	r, _ := d.Store.Get("grp/app#5")
	return r
}

func TestAnswerQuestionsCommentsAndEstimatesAgain(t *testing.T) {
	d, f, jobs := setup(t)
	calls := estimateClaude(t)
	ctx := context.Background()
	before := EstimateKey(newWithQuestions(t, d).Task)

	if err := d.AnswerQuestions(ctx, "grp/app#5", []Answer{{Q: "Яка саме кнопка?"}, {Q: "Який браузер?", A: "Safari 17"}}); err != nil {
		t.Fatal(err)
	}
	d.WaitEstimates()
	want := "**Відповіді на питання до задачі:**\n\n1. Який браузер? — Safari 17"
	if len(f.notes) != 1 || f.notes[0] != want {
		t.Fatalf("notes: %q", f.notes)
	}
	r, _ := d.Store.Get("grp/app#5")
	if len(r.Task.Comments) != 1 || r.Task.Comments[0].Body != want {
		t.Fatalf("comments: %+v", r.Task.Comments)
	}
	// The answers change what the estimate is for, and it ran again with them in its prompt.
	if key := EstimateKey(r.Task); key == before || r.Estimate.For != key || r.Estimate.State != "done" {
		t.Fatalf("estimate %+v, key %s (was %s)", r.Estimate, key, before)
	}
	if got := calls(); len(got) != 1 || !strings.Contains(got[0], "Safari_17") {
		t.Fatalf("calls: %v", got)
	}
	if !strings.Contains(orca.Prompt(r.Task, "GitLab"), "Safari 17") {
		t.Fatal("the agent's prompt has no answers")
	}

	// The next poll reads the comment back from the tracker: the same key, no estimate again.
	d.Poll(ctx)
	d.WaitEstimates()
	if r, _ := d.Store.Get("grp/app#5"); r.Estimate.For != EstimateKey(r.Task) || len(calls()) != 1 {
		t.Fatalf("estimated again after the poll: %d calls", len(calls()))
	}

	// The tracker fails to give the comments once: Mako keeps its own, the key stays, no estimate again.
	f.mu.Lock()
	f.notesDown = true
	f.mu.Unlock()
	d.Poll(ctx)
	d.WaitEstimates()
	if r, _ := d.Store.Get("grp/app#5"); len(r.Task.Comments) != 1 || len(calls()) != 1 {
		t.Fatalf("after a failed read of the comments: %+v, %d calls", r.Task.Comments, len(calls()))
	}

	// The task is read from the tracker before its start: the agent gets what changed meanwhile, and the
	// answers even when the comments fail to come again.
	f.mu.Lock()
	f.issues[5]["description"] = "Падає на кліку, у Safari теж"
	f.mu.Unlock()
	d.Take(ctx, "grp/app#5")
	d.Wait()
	if len(*jobs) != 1 || (*jobs)[0].Task.Description != "Падає на кліку, у Safari теж" || len((*jobs)[0].Task.Comments) != 1 {
		t.Fatalf("job's task: %+v", *jobs)
	}
}

func TestAnswerQuestionsTrackerError(t *testing.T) {
	d, f, _ := setup(t)
	calls := estimateClaude(t)
	newWithQuestions(t, d)
	f.failNotes = true

	err := d.AnswerQuestions(context.Background(), "grp/app#5", []Answer{{Q: "Яка саме кнопка?", A: "«Оплатити»"}})
	d.WaitEstimates()
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error: %v", err)
	}
	if r, _ := d.Store.Get("grp/app#5"); len(r.Task.Comments) != 0 || r.Estimate.State != "done" || len(calls()) != 0 {
		t.Fatalf("after a failed comment: %+v, %d calls", r, len(calls()))
	}
	if err := d.AnswerQuestions(context.Background(), "grp/app#5", []Answer{{Q: "Яка саме кнопка?", A: " "}}); err == nil {
		t.Fatal("sent without answers")
	}
}

// An answer written for a question the estimate no longer asks is not sent under another one.
func TestAnswerQuestionsChangedMeanwhile(t *testing.T) {
	d, f, _ := setup(t)
	newWithQuestions(t, d)
	err := d.AnswerQuestions(context.Background(), "grp/app#5", []Answer{{Q: "Яка саме кнопка?", A: "«Оплатити»"}, {Q: "Старе питання?", A: "так"}})
	if err == nil || !strings.Contains(err.Error(), "змінились") || len(f.notes) != 0 {
		t.Fatalf("error %v, notes %q", err, f.notes)
	}
}

func TestEstimateKeyCountsComments(t *testing.T) {
	task := core.Task{Title: "Кнопка", Description: "Падає"}
	with := task
	with.Comments = []core.Comment{{Author: "serg", Body: "**Відповіді:**\n\n1. Де? — тут"}}
	if EstimateKey(with) == EstimateKey(task) {
		t.Fatal("a comment did not change the key")
	}
	// What the tracker gives back may differ in its Markdown and spacing, not in its words.
	back := task
	back.Comments = []core.Comment{{Author: "Serg", Body: "Відповіді:\n1. Де? —  тут\n"}}
	if EstimateKey(back) != EstimateKey(with) {
		t.Fatal("the same words gave another key")
	}
	// An estimate made for the new key no longer fits once a comment comes.
	if estimateFits(&core.Estimate{For: EstimateKey(task)}, with) {
		t.Fatal("an estimate without the comment still fits")
	}
	// One made before comments counted still fits until the title or description changes.
	old := &core.Estimate{For: textKey(task)}
	changed := with
	changed.Description = "Падає двічі"
	if !estimateFits(old, with) || estimateFits(old, changed) {
		t.Fatal("an estimate of before the update")
	}
}
