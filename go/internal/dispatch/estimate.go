package dispatch

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// The estimate: a new task, as it appears in «Нові», gets a quick look from the agent (haiku, the task's text
// and the repository's layout, not its code): how clear it is and what to ask in the tracker, how big, how
// risky, and how to take it. Once per task; again only when its title, description or comments change, or on
// «Оцінити ще раз». A failure leaves the task without an estimate, quietly.

// EstimateKey: what the estimate of a task was made for; a changed title, description or comments (answers to
// its questions among them) make another. Comments count by their words (core.Plain), as a tracker may give
// back the Markdown of one Mako wrote otherwise. "2:" tells it from a key made before comments counted.
func EstimateKey(t core.Task) string {
	text := t.Title + "\n" + t.Description
	for _, c := range t.Comments {
		text += "\n\x00" + core.Plain(c.Body)
	}
	return "2:" + key(text)
}

// textKey: a key as made before comments counted, of the title and description only.
func textKey(t core.Task) string { return key(t.Title + "\n" + t.Description) }

func key(text string) string {
	h := sha1.Sum([]byte(text))
	return hex.EncodeToString(h[:6])
}

// estimateFits: the estimate was made for the task as it is. One made before comments counted still fits while
// the title and description stay: an update does not send every commented task in «Нові» to Opus again.
func estimateFits(e *core.Estimate, t core.Task) bool {
	return e != nil && (e.For == EstimateKey(t) || !strings.HasPrefix(e.For, "2:") && e.For == textKey(t))
}

// isNew: the task waits for «Взяти в роботу».
func isNew(r core.Record) bool { return r.Status == core.Asking || r.Status == core.Snoozed }

// maybeEstimate estimates a new task when the settings want it and it has no estimate of its current text.
func (d *Dispatcher) maybeEstimate(id string) {
	if !d.Config().Estimate {
		return
	}
	r, ok := d.Store.Get(id)
	if !ok || !isNew(r) {
		return
	}
	if e := r.Estimate; e != nil && (estimateFits(e, r.Task) || (e.State == "running" && !stale(e.At))) {
		return
	}
	if d.startEstimate(id) == nil {
		d.goEstimate(id)
	}
}

// Estimate: «Оцінити», whatever the settings say.
func (d *Dispatcher) Estimate(id string) error {
	if err := d.startEstimate(id); err != nil {
		return err
	}
	d.goEstimate(id)
	return nil
}

// goEstimate runs the estimate in the background; WaitEstimates waits for those running (tests).
func (d *Dispatcher) goEstimate(id string) {
	d.estimates.Add(1)
	go func() {
		defer d.estimates.Done()
		d.estimate(context.Background(), id)
	}()
}

func (d *Dispatcher) WaitEstimates() { d.estimates.Wait() }

// startEstimate marks the estimate as running, unless it cannot or need not start.
func (d *Dispatcher) startEstimate(id string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return errors.New("немає такої задачі")
	}
	if !isNew(r) {
		return errors.New("оцінити наперед можна лише нову задачу")
	}
	if e := r.Estimate; e != nil && e.State == "running" && !stale(e.At) {
		return errors.New("задачу вже оцінюють")
	}
	key := EstimateKey(r.Task)
	d.Store.Update(id, func(r *core.Record) {
		// The last estimate stays readable meanwhile, for the size and the order of «Нові».
		e := core.Estimate{}
		if r.Estimate != nil && r.Estimate.State == "done" {
			e = *r.Estimate
		}
		e.At, e.State, e.For, e.Error = now(), "running", key, ""
		r.Estimate = &e
	})
	return nil
}

// estimate asks the agent for the estimate and keeps it with the key of the text it read.
func (d *Dispatcher) estimate(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, writer.EstimateTimeout+time.Minute) // the estimate's own limit, and a minute to read the repository
	defer cancel()
	r, ok := d.Store.Get(id)
	if !ok {
		return
	}
	dir := os.TempDir()
	if t := d.tracker(r.Task.TrackerID); t != nil {
		if repo, err := d.route(t, r.Task); err == nil && exists(repo.Path) {
			dir = repo.Path
		}
	}
	key := EstimateKey(r.Task)
	e, err := writer.Estimate(d.spending(ctx, id, core.PhaseEstimate), dir, r.Task, writer.RepoOutline(ctx, dir))
	e.At, e.For = now(), key
	if err != nil {
		// Quietly: the task just has no estimate. The journal says why.
		d.Log(id, "the task was not estimated upfront: "+err.Error())
		e = core.Estimate{At: e.At, For: key, State: "failed", Error: err.Error()}
	} else {
		d.Log(id, "the agent estimated the task upfront: "+sizeName[e.Size])
	}
	d.Store.Update(id, func(r *core.Record) { r.Estimate = &e })
}

var sizeName = map[string]string{"small": "small", "medium": "medium", "big": "large"}
