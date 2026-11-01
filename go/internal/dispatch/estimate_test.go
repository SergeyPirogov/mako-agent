package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// estimateClaude answers every prompt with an estimate and keeps each call's arguments and prompt, one per line of the log.
func estimateClaude(t *testing.T) func() []string {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\n(echo \"$*\"; cat; echo) | tr '\\n' ' ' >> " + calls + "\necho >> " + calls + "\n" +
		`echo '{"clarity":{"score":4,"why":"зрозуміло"},"questions":["Яка саме кнопка?"],"size":"big","parts":"web","risk":{"score":2,"why":"UI"},"plan":true,"split":false,"model":"sonnet","advice":"почати з плану"}'` + "\n"
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755)
	old := writer.Claude
	writer.Claude = filepath.Join(dir, "claude")
	t.Cleanup(func() { writer.Claude = old })
	return func() []string {
		b, _ := os.ReadFile(calls)
		return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), " ", "_"))
	}
}

func TestEstimateNewTask(t *testing.T) {
	d, f, _ := setup(t)
	calls := estimateClaude(t)
	d.Cfg.Estimate = true
	ctx := context.Background()

	d.Poll(ctx)
	d.WaitEstimates()
	r, _ := d.Store.Get("grp/app#5")
	e := r.Estimate
	if e == nil || e.State != "done" || e.Size != "big" || e.Clarity.Value != 4 || e.Risk.Why != "UI" || !e.Plan || e.Model != "sonnet" ||
		!strings.Contains(e.Questions, "Яка саме кнопка?") || e.For != estimateKey(r.Task) {
		t.Fatalf("estimate: %+v", e)
	}
	// Opus estimates the task, and so picks who does it.
	if got := calls(); len(got) != 1 || !strings.Contains(got[0], "Кнопка") || !strings.Contains(got[0], "--model_opus") {
		t.Fatalf("calls: %v", got)
	}

	// The same text on the next poll: no new estimate.
	d.Poll(ctx)
	d.WaitEstimates()
	if n := len(calls()); n != 1 {
		t.Fatalf("estimated again: %d calls", n)
	}

	// A changed description: estimated again.
	f.mu.Lock()
	f.issues[5]["description"] = "Падає на кліку, у Safari теж"
	f.mu.Unlock()
	d.Poll(ctx)
	d.WaitEstimates()
	if n := len(calls()); n != 2 {
		t.Fatalf("a changed description: %d calls", n)
	}
	r, _ = d.Store.Get("grp/app#5")
	if r.Estimate.For != estimateKey(r.Task) || r.Estimate.State != "done" {
		t.Fatalf("estimate of the new text: %+v", r.Estimate)
	}

	// «Оцінити ще раз» works on demand; a taken task is not estimated before the start.
	if err := d.Estimate("grp/app#5"); err != nil {
		t.Fatal(err)
	}
	d.WaitEstimates()
	if n := len(calls()); n != 3 {
		t.Fatalf("on demand: %d calls", n)
	}
	d.Store.Update("grp/app#5", func(r *core.Record) { r.Status = core.Done })
	if d.Estimate("grp/app#5") == nil {
		t.Fatal("a done task estimated before the start")
	}
}

func TestEstimateOff(t *testing.T) {
	d, _, _ := setup(t)
	calls := estimateClaude(t)
	d.Poll(context.Background())
	d.WaitEstimates()
	if r, _ := d.Store.Get("grp/app#5"); r.Estimate != nil || len(calls()) != 0 {
		t.Fatalf("estimated with the setting off: %+v", r.Estimate)
	}
}

func TestEstimateFailsQuietly(t *testing.T) {
	d, _, _ := setup(t) // no Claude
	d.Cfg.Estimate = true
	ctx := context.Background()
	d.Poll(ctx)
	d.WaitEstimates()
	r, _ := d.Store.Get("grp/app#5")
	if r.Estimate == nil || r.Estimate.State != "failed" || r.Status != core.Snoozed {
		t.Fatalf("estimate: %+v", r.Estimate)
	}
	if len(d.Faults()) != 0 {
		t.Fatalf("a failed estimate is a fault: %+v", d.Faults())
	}
	// Not tried again on every poll.
	estimateClaude(t)
	d.Poll(ctx)
	d.WaitEstimates()
	if r, _ := d.Store.Get("grp/app#5"); r.Estimate.State != "failed" {
		t.Fatal("a failed estimate is retried by itself")
	}
}
