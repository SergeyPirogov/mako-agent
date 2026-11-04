package dispatch

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// selfSetup: Mako's agent on grp/app#5 with its branch pushed, an open MR in the fake GitLab and an assessment
// that asks to check steps; the MR looks ready to merge to the board.
func selfSetup(t *testing.T, steps []core.CheckStep, prep ...func(*config.Config)) (d *Dispatcher, cfg *config.Config, id, wt string, f *fakeGitLab) {
	d, cfg, id, wt, _, f = reviewSetup(t, prep...)
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "Pay")
	git(t, wt, "push", "-q", "origin", "fix/5-pay")
	// From now on the repository's origin is the fake GitLab, which has the MR (the pushes are done).
	for _, r := range d.Config().Trackers[0].Projects["grp/app"].Repos {
		git(t, r.Path, "remote", "set-url", "origin", f.base+"/grp/app.git")
	}
	url := fmt.Sprintf("%s/grp/app/-/merge_requests/1", f.base)
	f.mu.Lock()
	f.mrs = append(f.mrs, map[string]any{"iid": 1, "state": "opened", "source_branch": "fix/5-pay", "target_branch": "main", "title": "Pay", "description": "## Що змінено\n- оплата", "web_url": url})
	f.mu.Unlock()
	d.Store.Update(id, func(r *core.Record) {
		r.Status, r.PRURL = core.Done, url
		r.Assessment = &core.Assessment{At: now(), State: "done", Review: &core.ReviewAdvice{Need: false, Steps: steps}}
	})
	d.reviews = map[string]reviewLook{id: {at: time.Now(), check: &trackers.ReviewCheck{State: "open", CI: "success", Mergeable: true}}}
	return d, cfg, id, wt, f
}

func headIn(t *testing.T, wt string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// lookAgain: the board's look at the branch is kept for 15 seconds; a test that commits looks again at once.
func lookAgain(d *Dispatcher, id string) {
	d.mu.Lock()
	delete(d.branches, id)
	d.mu.Unlock()
}

var threeSteps = []core.CheckStep{{Open: "/settings", Do: "Зберегти проєкт", Expect: "Збережено"}, {Open: "/", Do: "Відкрити задачу"}, {Do: "Змерджити"}}

// Each step is ticked at the branch's HEAD; all of them ticked is the whole check; new commits make every tick
// stale; a step the assessment no longer has loses its tick.
func TestSelfCheckSteps(t *testing.T) {
	d, _, id, wt, _ := selfSetup(t, threeSteps)
	r, _ := d.Store.Get(id)
	v := d.SelfView(r)
	if v == nil || !v.Required || v.Left != 3 || v.Done || !v.Blocks || len(v.Steps) != 3 || v.Steps[0].Open != "/settings" {
		t.Fatalf("view %+v", v)
	}
	if err := d.SelfStep(id, 3, true, ""); err == nil {
		t.Fatal("a step that is not there")
	}
	if err := d.SelfStep(id, 0, true, ""); err != nil {
		t.Fatal(err)
	}
	r, _ = d.Store.Get(id)
	head := headIn(t, wt)
	if v = d.SelfView(r); v.Left != 2 || !v.Steps[0].Done || r.SelfCheck.Commit != "" || r.SelfCheck.Steps[0].Commit != head {
		t.Fatalf("one ticked: %+v, %+v", v, r.SelfCheck)
	}
	for _, n := range []int{1, 2} {
		if err := d.SelfStep(id, n, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	r, _ = d.Store.Get(id)
	if v = d.SelfView(r); v.Left != 0 || !v.Done || v.Blocks || r.SelfCheck.Commit != head || r.SelfCheck.At == "" {
		t.Fatalf("all ticked: %+v, %+v", v, r.SelfCheck)
	}

	// A commit after the ticks: each of them is a commit behind, and the check is to be done again.
	git(t, wt, "commit", "-q", "--allow-empty", "-m", "More")
	lookAgain(d, id)
	if v = d.SelfView(r); v.Left != 3 || v.Done || !v.Blocks || v.Steps[1].Stale != 1 || d.SelfCheckBehind(r) != 1 {
		t.Fatalf("stale: %+v", v)
	}
	if err := d.SelfStep(id, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	r, _ = d.Store.Get(id)
	if v = d.SelfView(r); v.Left != 2 || v.Steps[1].Stale != 0 || v.Steps[0].Stale != 1 {
		t.Fatalf("one ticked again: %+v", v)
	}

	// The assessment made anew with another first step: its tick is gone, the others stay.
	d.Store.Update(id, func(r *core.Record) {
		r.Assessment.Review.Steps = []core.CheckStep{{Do: "Інший крок"}, threeSteps[1], threeSteps[2]}
	})
	r, _ = d.Store.Get(id)
	if v = d.SelfView(r); v.Steps[0].Done || !v.Steps[1].Done || v.Steps[0].Do != "Інший крок" {
		t.Fatalf("new steps: %+v", v)
	}

	// Taking a tick away, and every tick at once.
	if err := d.SelfStep(id, 1, false, ""); err != nil {
		t.Fatal(err)
	}
	if r, _ = d.Store.Get(id); d.SelfView(r).Steps[1].Done {
		t.Fatal("the tick stayed")
	}
	if err := d.SelfCheck(id, true); err != nil {
		t.Fatal(err)
	}
	if r, _ = d.Store.Get(id); !d.SelfView(r).Done {
		t.Fatalf("all at once: %+v", d.SelfView(r))
	}

	// A tick from before the steps (only its time and commit) ticks every step at that commit.
	d.Store.Update(id, func(r *core.Record) { r.SelfCheck = &core.SelfCheck{At: now(), Commit: headIn(t, wt)} })
	if r, _ = d.Store.Get(id); !d.SelfView(r).Done {
		t.Fatalf("old tick: %+v", d.SelfView(r))
	}
}

// The check holds the merge (the board's and Mako's own readiness) until its steps are ticked or it is skipped
// with a reason; the stats learn how the MR went in.
func TestSelfCheckHoldsMerge(t *testing.T) {
	d, _, id, _, f := selfSetup(t, threeSteps[:2])
	r, _ := d.Store.Get(id)
	if d.MergeReady(r) {
		t.Fatal("ready with the check not done")
	}
	if left, total, blocks := d.SelfBlocks(r); !blocks || left != 2 || total != 2 {
		t.Fatalf("blocks %d of %d, %v", left, total, blocks)
	}
	err := d.MergeReview(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "check it yourself: 2 of 2 steps left") {
		t.Fatalf("merged: %v", err)
	}
	if err := d.SelfSkip(id, "  "); err == nil {
		t.Fatal("skipped without a reason")
	}
	if err := d.SelfSkip(id, "лише документація"); err != nil {
		t.Fatal(err)
	}
	r, _ = d.Store.Get(id)
	if v := d.SelfView(r); v.Blocks || v.Skipped != "лише документація" || !d.MergeReady(r) {
		t.Fatalf("skipped: %+v", v)
	}
	if !journalHas(d, id, "self-check skipped: лише документація") {
		t.Fatalf("journal %+v", d.Logs(id))
	}
	if err := d.MergeReview(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	state := f.mrs[0]["state"]
	f.mu.Unlock()
	if r, _ = d.Store.Get(id); state != "merged" || r.CheckedAtMerge != "skipped" {
		t.Fatalf("MR %v, checked at merge %q", state, r.CheckedAtMerge)
	}
}

// Ticked, the MR merges as checked.
func TestSelfCheckMergesChecked(t *testing.T) {
	d, _, id, _, _ := selfSetup(t, threeSteps[:1])
	if err := d.SelfStep(id, 0, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.MergeReview(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.Store.Get(id); r.CheckedAtMerge != "checked" {
		t.Fatalf("checked at merge %q", r.CheckedAtMerge)
	}
}

// The setting: "off" leaves the steps as advice; "always" asks for a look also when the agent advises none.
func TestSelfCheckModes(t *testing.T) {
	d, cfg, id, _, _ := selfSetup(t, threeSteps[:1])
	cfg.SelfCheck = config.SelfCheckOff
	r, _ := d.Store.Get(id)
	if v := d.SelfView(r); v == nil || v.Required || v.Blocks || !d.MergeReady(r) {
		t.Fatalf("off: %+v", v)
	}
	cfg.SelfCheck = config.SelfCheckAdvised
	d.Store.Update(id, func(r *core.Record) { r.Assessment.Review = &core.ReviewAdvice{Need: true, Why: "ризиково"} })
	if r, _ = d.Store.Get(id); d.SelfView(r) != nil || !d.MergeReady(r) {
		t.Fatalf("advised, no advice: %+v", d.SelfView(r))
	}
	cfg.SelfCheck = config.SelfCheckAlways
	v := d.SelfView(r)
	if v == nil || !v.Blocks || len(v.Steps) != 1 || v.Steps[0].Do == "" || d.MergeReady(r) {
		t.Fatalf("always: %+v", v)
	}
	// While the agent assesses, nothing is asked yet (Assessing holds the merge by itself).
	d.Store.Update(id, func(r *core.Record) { r.Assessment = &core.Assessment{At: now(), State: "running"} })
	if r, _ = d.Store.Get(id); d.SelfView(r) != nil {
		t.Fatal("asked while assessing")
	}
}

func journalHas(d *Dispatcher, id, text string) bool {
	for _, l := range d.Logs(id) {
		if strings.Contains(l.Text, text) {
			return true
		}
	}
	return false
}
