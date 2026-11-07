package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// intakeAgent is a fake `claude` for the intake: it answers with what say() last set, and keeps each call's
// arguments and prompt. The tests' origin is out of reach: fetching the base branch fails at once.
type intakeAgent struct{ dir string }

func newIntakeAgent(t *testing.T, d *Dispatcher, mode string) *intakeAgent {
	t.Helper()
	t.Setenv("GIT_SSH_COMMAND", "false")
	dir := t.TempDir()
	script := "#!/bin/sh\nn=$(ls " + dir + " | grep -c 'args$')\necho \"$*\" > " + dir + "/call$n.args\ncat > " + dir + "/call$n.prompt\n" +
		"if [ -f " + dir + "/sleep ]; then sleep 5; fi\nif [ -f " + dir + "/fail ]; then echo 'boom' >&2; exit 1; fi\ncat " + dir + "/answer\n"
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755)
	old := writer.Claude
	writer.Claude = filepath.Join(dir, "claude")
	t.Cleanup(func() { writer.Claude = old })
	d.Cfg.Intake = mode
	return &intakeAgent{dir: dir}
}

func (a *intakeAgent) say(answer string) {
	os.WriteFile(filepath.Join(a.dir, "answer"), []byte(answer), 0o644)
}

// calls: the arguments and the prompt of each call, in order.
func (a *intakeAgent) calls() [][2]string {
	var out [][2]string
	for n := 0; ; n++ {
		args, err := os.ReadFile(filepath.Join(a.dir, "call"+strconv.Itoa(n)+".args"))
		if err != nil {
			return out
		}
		prompt, _ := os.ReadFile(filepath.Join(a.dir, "call"+strconv.Itoa(n)+".prompt"))
		out = append(out, [2]string{string(args), string(prompt)})
	}
}

func takeAndWait(d *Dispatcher, id string) core.Record {
	d.Take(context.Background(), id)
	d.Wait()
	r, _ := d.Store.Get(id)
	return r
}

// «take»: the task goes to work as without the intake; the intake ran with tools that only read, on its route,
// with the prompt in Mako's language and the task as data.
func TestIntakeTake(t *testing.T) {
	d, f, jobs := setup(t)
	a := newIntakeAgent(t, d, config.IntakeAlways)
	a.say(`{"decision":"take","why":"ясно, що робити"}`)
	d.Poll(context.Background())

	r := takeAndWait(d, "grp/app#5")
	if r.Status != core.HandedOff || len(*jobs) != 1 {
		t.Fatalf("status %s, jobs %d, error %q", r.Status, len(*jobs), r.Error)
	}
	if i := r.Intake; i == nil || i.State != "done" || i.Decision != core.IntakeTake || i.Why != "ясно, що робити" || i.For != textKey(r.Task) {
		t.Fatalf("intake %+v", r.Intake)
	}
	if len(f.notes) != 1 || !strings.HasPrefix(f.notes[0], "Взяв задачу в роботу") {
		t.Fatalf("notes %v", f.notes)
	}
	calls := a.calls()
	if len(calls) != 1 {
		t.Fatalf("calls %d", len(calls))
	}
	args, prompt := calls[0][0], calls[0][1]
	for _, want := range []string{"--permission-mode default", "--disallowedTools Edit Write MultiEdit NotebookEdit", "--allowedTools Read Grep Glob", "Bash(git grep:*)", "--model haiku --effort medium"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q: no %q", args, want)
		}
	}
	for _, want := range []string{"## Intake: чи є що робити", "origin/main", "\"split\"", "Кнопка оплати не працює", core.UntrustedNote} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt: no %q", want)
		}
	}
	if r.Metrics == nil || r.Metrics.Routes[core.PhaseIntake].Model != "haiku" {
		t.Fatalf("intake route not kept: %+v", r.Metrics)
	}
}

// «clarify»: the questions go to the tracker and the task back to «Нові», with no branch, attempt or status; the
// comment Mako wrote is no answer, the next one is: Mako takes the task again, and the intake looks anew.
func TestIntakeClarifyThenAnswerCame(t *testing.T) {
	d, f, jobs := setup(t)
	a := newIntakeAgent(t, d, config.IntakeAlways)
	a.say(`{"decision":"clarify","why":"не сказано, яка кнопка","questions":["Яка саме кнопка: на картці чи в кошику?",""]}`)
	ctx := context.Background()
	d.Poll(ctx)

	r := takeAndWait(d, "grp/app#5")
	if r.Status != core.Asking || len(*jobs) != 0 || r.StartedAt != "" || r.BranchName != "" || r.Attempt != 0 {
		t.Fatalf("status %s, jobs %d, %+v", r.Status, len(*jobs), r)
	}
	if len(f.notes) != 1 || f.notes[0] != i18n.TA("comment.intakeClarify", "- Яка саме кнопка: на картці чи в кошику?") {
		t.Fatalf("notes %q", f.notes)
	}
	if len(f.statusSet) != 0 || len(f.assigned) != 0 {
		t.Fatalf("the issue was moved or assigned: %v %v", f.statusSet, f.assigned)
	}
	if i := r.Intake; i == nil || i.Decision != core.IntakeClarify || len(i.Questions) != 1 || i.Comments != 1 || len(r.Task.Comments) != 1 {
		t.Fatalf("intake %+v, comments %d", r.Intake, len(r.Task.Comments))
	}
	if r.Column(time.Now().UnixMilli()) != "new" {
		t.Fatalf("column %s", r.Column(time.Now().UnixMilli()))
	}

	// The tracker gives Mako's own comment back: not an answer.
	d.Poll(ctx)
	d.Wait()
	if r, _ = d.Store.Get("grp/app#5"); r.Status != core.Asking || len(a.calls()) != 1 {
		t.Fatalf("taken again on Mako's own comment: %s, %d calls", r.Status, len(a.calls()))
	}

	// Someone answers: taken again, and this time the intake takes it.
	f.mu.Lock()
	f.notesOf[5] = append(f.notesOf[5], "На картці товару")
	f.issues[5]["user_notes_count"] = len(f.notesOf[5])
	f.mu.Unlock()
	a.say(`{"decision":"take","why":"тепер ясно"}`)
	d.Poll(ctx)
	d.Wait()
	if r, _ = d.Store.Get("grp/app#5"); r.Status != core.HandedOff || len(*jobs) != 1 || len(a.calls()) != 2 {
		t.Fatalf("status %s, jobs %d, calls %d", r.Status, len(*jobs), len(a.calls()))
	}
	if !strings.Contains(a.calls()[1][1], "На картці товару") || r.Intake.Decision != core.IntakeTake {
		t.Fatalf("the second intake did not read the answer: %+v", r.Intake)
	}
}

// Taking the task again while the intake's decision stands takes it anyway: no second intake.
func TestIntakeTakeAnyway(t *testing.T) {
	d, _, jobs := setup(t)
	a := newIntakeAgent(t, d, config.IntakeAlways)
	a.say(`{"decision":"clarify","why":"неясно","questions":["Що саме?"]}`)
	d.Poll(context.Background())
	if r := takeAndWait(d, "grp/app#5"); r.Status != core.Asking {
		t.Fatalf("status %s", r.Status)
	}
	d.Poll(context.Background()) // the comment comes back from the tracker: the decision still stands
	r := takeAndWait(d, "grp/app#5")
	if r.Status != core.HandedOff || len(*jobs) != 1 || len(a.calls()) != 1 {
		t.Fatalf("status %s, jobs %d, calls %d", r.Status, len(*jobs), len(a.calls()))
	}
	if !r.Intake.Overridden || r.Intake.Decision != core.IntakeClarify || r.Intake.Filtered() {
		t.Fatalf("intake %+v", r.Intake)
	}
}

// «answer»: the answer goes to the tracker and the task is closed there, done with no branch and no MR; reopened
// in the tracker, it is back in «Нові».
func TestIntakeAnswer(t *testing.T) {
	d, f, jobs := setup(t)
	a := newIntakeAgent(t, d, config.IntakeAlways)
	a.say(`{"decision":"answer","why":"це питання","answer":"Кнопка в ` + "`web/pay.go:12`" + `, баг уже виправлено."}`)
	ctx := context.Background()
	d.Poll(ctx)

	r := takeAndWait(d, "grp/app#5")
	if r.Status != core.Done || len(*jobs) != 0 || r.BranchName != "" || r.PRURL != "" || r.FinishedAt == "" {
		t.Fatalf("status %s, jobs %d, %+v", r.Status, len(*jobs), r)
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "`web/pay.go:12`") || f.notes[0] != i18n.TA("comment.intakeAnswer", r.Intake.Answer) {
		t.Fatalf("notes %q", f.notes)
	}
	if f.issues[5]["state"] != "closed" || !slices.Contains(f.statusSet, "Done") || !r.Intake.Closed || !r.Intake.Filtered() {
		t.Fatalf("not closed: state %v, statuses %v, intake %+v", f.issues[5]["state"], f.statusSet, r.Intake)
	}
	if r.Column(time.Now().UnixMilli()) != "done" {
		t.Fatalf("column %s", r.Column(time.Now().UnixMilli()))
	}

	// Closed, the task is not in the search: nothing changes.
	d.Poll(ctx)
	if r, _ = d.Store.Get("grp/app#5"); r.Status != core.Done {
		t.Fatalf("status %s", r.Status)
	}
	// Reopened: back in «Нові»; taken, it goes to work past the answer (nobody commented since).
	f.mu.Lock()
	f.issues[5]["state"] = "opened"
	f.mu.Unlock()
	d.Poll(ctx)
	if r, _ = d.Store.Get("grp/app#5"); r.Status != core.Snoozed || r.Intake.Closed {
		t.Fatalf("not reopened: %s, %+v", r.Status, r.Intake)
	}
	if r = takeAndWait(d, "grp/app#5"); r.Status != core.HandedOff || len(a.calls()) != 1 || !r.Intake.Overridden {
		t.Fatalf("status %s, calls %d, intake %+v", r.Status, len(a.calls()), r.Intake)
	}
}

// An answer Mako could not post is not lost unsaid: the task goes to work.
func TestIntakeAnswerCommentFails(t *testing.T) {
	d, f, jobs := setup(t)
	a := newIntakeAgent(t, d, config.IntakeAlways)
	a.say(`{"decision":"answer","why":"питання","answer":"Ось відповідь"}`)
	f.failNotes = true
	d.Poll(context.Background())
	r := takeAndWait(d, "grp/app#5")
	if r.Status != core.HandedOff || len(*jobs) != 1 || r.Intake.Decision != core.IntakeTake || f.issues[5]["state"] != "opened" {
		t.Fatalf("status %s, jobs %d, intake %+v", r.Status, len(*jobs), r.Intake)
	}
}

// «split»: the task goes back to «Нові» and the agent proposes its subtasks.
func TestIntakeSplit(t *testing.T) {
	d, f, jobs := setup(t)
	a := newIntakeAgent(t, d, config.IntakeAlways)
	a.say(`{"decision":"split","why":"тут три задачі"}`)
	asked := splitAgent(t, threeSubtasks, nil)
	d.Poll(context.Background())

	r := takeAndWait(d, "grp/app#5")
	d.WaitSplits()
	r, _ = d.Store.Get("grp/app#5")
	if r.Status != core.Asking || len(*jobs) != 0 || len(f.notes) != 0 || r.StartedAt != "" {
		t.Fatalf("status %s, jobs %d, notes %v", r.Status, len(*jobs), f.notes)
	}
	if *asked != 1 || r.Breakdown == nil || r.Breakdown.State != "done" || len(r.Breakdown.Items) != 3 || r.Intake.Decision != core.IntakeSplit {
		t.Fatalf("asked %d, breakdown %+v, intake %+v", *asked, r.Breakdown, r.Intake)
	}
}

// The intake failed, or answered with what Mako cannot read: the task goes to work as without it.
func TestIntakeFailsGoesToWork(t *testing.T) {
	for name, set := range map[string]func(a *intakeAgent){
		"fails":   func(a *intakeAgent) { os.WriteFile(filepath.Join(a.dir, "fail"), nil, 0o644) },
		"unknown": func(a *intakeAgent) { a.say(`{"decision":"maybe","why":"?"}`) },
		"notjson": func(a *intakeAgent) { a.say("не знаю") },
		"clarify with no questions": func(a *intakeAgent) {
			a.say(`{"decision":"clarify","why":"?","questions":[]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, f, jobs := setup(t)
			a := newIntakeAgent(t, d, config.IntakeAlways)
			set(a)
			d.Poll(context.Background())
			r := takeAndWait(d, "grp/app#5")
			if r.Status != core.HandedOff || len(*jobs) != 1 {
				t.Fatalf("status %s, jobs %d, error %q", r.Status, len(*jobs), r.Error)
			}
			if r.Intake == nil || r.Intake.State != "failed" || r.Intake.Error == "" || r.Intake.Filtered() {
				t.Fatalf("intake %+v", r.Intake)
			}
			if len(f.notes) != 1 || !strings.HasPrefix(f.notes[0], "Взяв задачу в роботу") {
				t.Fatalf("notes %v", f.notes)
			}
		})
	}
}

// With the intake off, or a clear small task with "unclear", the agent is not asked.
func TestIntakeNotAsked(t *testing.T) {
	clear := func(r *core.Record) {
		r.Estimate = &core.Estimate{State: "done", For: EstimateKey(r.Task), Size: "small", Clarity: core.Score{Value: 5}}
	}
	for name, c := range map[string]struct {
		mode   string
		record func(r *core.Record)
	}{
		"never":           {config.IntakeNever, func(*core.Record) {}},
		"clear and small": {config.IntakeUnclear, clear},
		"a retry":         {config.IntakeAlways, func(r *core.Record) { r.RetryNote = "інакше" }},
		"a subtask":       {config.IntakeAlways, func(r *core.Record) { r.ParentID = "grp/app#1" }},
	} {
		t.Run(name, func(t *testing.T) {
			d, _, jobs := setup(t)
			a := newIntakeAgent(t, d, c.mode)
			a.say(`{"decision":"clarify","why":"?","questions":["?"]}`)
			d.Poll(context.Background())
			d.Store.Update("grp/app#5", c.record)
			r := takeAndWait(d, "grp/app#5")
			if r.Status != core.HandedOff || len(*jobs) != 1 || len(a.calls()) != 0 || r.Intake != nil {
				t.Fatalf("status %s, jobs %d, calls %d, intake %+v", r.Status, len(*jobs), len(a.calls()), r.Intake)
			}
		})
	}
}

// "unclear": an unclear or big task, or one with no estimate of its text, goes through the intake.
func TestWantsIntake(t *testing.T) {
	task := core.Task{Title: "Кнопка", Description: "Падає"}
	est := func(size string, clarity int) *core.Estimate {
		return &core.Estimate{State: "done", For: EstimateKey(task), Size: size, Clarity: core.Score{Value: clarity}}
	}
	for name, c := range map[string]struct {
		mode string
		r    core.Record
		want bool
	}{
		"never":           {config.IntakeNever, core.Record{Task: task}, false},
		"always":          {config.IntakeAlways, core.Record{Task: task, Estimate: est("small", 5)}, true},
		"no estimate":     {config.IntakeUnclear, core.Record{Task: task}, true},
		"stale estimate":  {config.IntakeUnclear, core.Record{Task: task, Estimate: &core.Estimate{State: "done", For: "old", Size: "small", Clarity: core.Score{Value: 5}}}, true},
		"unclear":         {config.IntakeUnclear, core.Record{Task: task, Estimate: est("small", 2)}, true},
		"big":             {config.IntakeUnclear, core.Record{Task: task, Estimate: est("big", 5)}, true},
		"clear and small": {config.IntakeUnclear, core.Record{Task: task, Estimate: est("small", 5)}, false},
		"plan label":      {config.IntakeUnclear, core.Record{Task: core.Task{Title: "Кнопка", Labels: []string{"plan"}}}, true},
		"started before":  {config.IntakeAlways, core.Record{Task: task, StartedAt: "2026-01-01T00:00:00Z"}, false},
		"from a plan":     {config.IntakeAlways, core.Record{Task: task, NoPlan: true}, false},
		"written by Mako": {config.IntakeAlways, core.Record{Task: task, CreatedBy: "ci"}, false},
	} {
		if got := wantsIntake(c.mode, c.r); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// «Скасувати» while the intake runs: the task is only cancelled, nothing posted.
func TestIntakeCancelled(t *testing.T) {
	d, f, jobs := setup(t)
	a := newIntakeAgent(t, d, config.IntakeAlways)
	a.say(`{"decision":"clarify","why":"?","questions":["?"]}`)
	os.WriteFile(filepath.Join(a.dir, "sleep"), nil, 0o644)
	d.Poll(context.Background())
	d.Take(context.Background(), "grp/app#5")
	for deadline := time.Now().Add(5 * time.Second); len(a.calls()) == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	d.Cancel("grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
	if r.Status != core.Cancelled || len(*jobs) != 0 || len(f.notes) != 0 || r.Intake != nil {
		t.Fatalf("status %s, jobs %d, notes %v, intake %+v", r.Status, len(*jobs), f.notes, r.Intake)
	}
}

// Auto-take leaves a task the intake decided against in «Нові»: only you take it anyway.
func TestAutoTakeSkipsIntakeDecision(t *testing.T) {
	d, _, _ := setup(t)
	d.Poll(context.Background())
	d.Cfg.Notifications.AutoTake = true
	d.Store.Update("grp/app#5", func(r *core.Record) {
		r.Status = core.Asking
		r.Intake = &core.Intake{State: "done", Decision: core.IntakeClarify, For: textKey(r.Task), Comments: len(r.Task.Comments)}
	})
	d.autoTake(context.Background(), "grp/app#5")
	r, _ := d.Store.Get("grp/app#5")
	if r.Status != core.Asking || r.AutoSkip != "the intake decided: clarify" {
		t.Fatalf("status %s, skip %q", r.Status, r.AutoSkip)
	}
}
