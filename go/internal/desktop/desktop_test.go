package desktop

import (
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

func TestSnapshot(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	now := time.Now()
	store.Update("a#1", func(r *core.Record) {
		r.Status, r.Task = core.Snoozed, core.Task{Title: "Нова задача з дуже довгою назвою, що не влізе в меню цілком"}
	})
	store.Update("a#2", func(r *core.Record) {
		r.Status, r.PostponedByYou, r.SnoozedUntil, r.Task = core.Snoozed, true, now.Add(time.Hour).UnixMilli(), core.Task{Title: "Відкладена"}
	})
	store.Update("a#3", func(r *core.Record) { r.Status, r.Task = core.HandedOff, core.Task{Title: "В Orca"} })
	store.Update("a#4", func(r *core.Record) { r.Status, r.PRURL, r.Task = core.Done, "u", core.Task{Title: "На рев'ю"} })
	d := dispatch.New(&config.Config{WipLimit: 2}, store, nil)
	v := Snapshot(d, now)
	if v.NewCount != 1 || v.Title != "1" || !v.Attention || v.Headline != "Mako · 1 of 2 at work" {
		t.Fatalf("%+v", v)
	}
	if len(v.Items) != 3 || v.Items[0].ID != "a#1" || !strings.HasSuffix(v.Items[0].Label, "… — new") {
		t.Fatalf("items %+v", v.Items)
	}
	d.SetPaused(true)
	if v := Snapshot(d, now); !strings.Contains(v.Line, "paused") {
		t.Fatalf("paused line %q", v.Line)
	}
}
