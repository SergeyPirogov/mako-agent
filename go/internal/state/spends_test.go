package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestSpends(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSpends(dir, "spend.json")
	if err != nil {
		t.Fatal(err)
	}
	s.Add(core.Spend{At: "2026-11-01T10:00:00Z", Phase: core.PhaseForMe, Project: "grp/app", USD: 0.5, Usage: core.Usage{Input: 10}})
	s.Add(core.Spend{At: "2026-11-01T11:00:00Z", Phase: core.PhaseForMe}) // nothing spent: not kept
	again, _ := OpenSpends(dir, "spend.json")
	if all := again.All(); len(all) != 1 || all[0].USD != 0.5 || all[0].Usage.Input != 10 || all[0].Project != "grp/app" {
		t.Fatalf("%+v", all)
	}
	for i := 0; i < maxSpends+5; i++ {
		again.Add(core.Spend{Phase: core.PhaseForMe, USD: float64(i + 1)})
	}
	if all := again.All(); len(all) != maxSpends || all[len(all)-1].USD != maxSpends+5 {
		t.Fatalf("kept %d, last %+v", len(all), all[len(all)-1])
	}
	// A broken file: kept aside, the list starts anew.
	os.WriteFile(filepath.Join(dir, "spend.json"), []byte("{"), 0o600)
	broken, err := OpenSpends(dir, "spend.json")
	if err != nil || len(broken.All()) != 0 {
		t.Fatalf("%v %+v", err, broken.All())
	}
	if _, err := os.Stat(filepath.Join(dir, "spend.json.broken")); err != nil {
		t.Fatal(err)
	}
}
