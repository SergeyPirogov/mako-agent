package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
)

// A state.json that could not be read is one of Mako's errors on the board, not only a line in the log; a good
// one is not.
func TestBrokenStateIsAFault(t *testing.T) {
	for _, broken := range []bool{false, true} {
		dir := t.TempDir()
		if broken {
			os.WriteFile(filepath.Join(dir, "state.json"), []byte("{broken"), 0o600)
		}
		store, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		d := dispatch.New(&config.Config{StateDir: dir}, store, nil)
		reportBrokenState(d, store)
		faults := d.Faults()
		if !broken {
			if len(faults) != 0 {
				t.Fatalf("faults for a good state.json: %+v", faults)
			}
			continue
		}
		if len(faults) != 1 || faults[0].What != "state.json broken" || faults[0].Task != "mako" ||
			!strings.Contains(faults[0].Error, store.Broken) {
			t.Fatalf("faults %+v", faults)
		}
	}
}
