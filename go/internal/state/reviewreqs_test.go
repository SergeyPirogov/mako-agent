package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// A broken review-requests.json does not turn «Рев'ю для мене» off: it is kept aside and the list starts empty.
func TestBrokenReviewRequests(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "review-requests.json"), []byte("{broken"), 0o644)
	s, err := OpenReviewRequests(dir)
	if err != nil || s == nil || len(s.All()) != 0 {
		t.Fatalf("%v %v", s, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "review-requests.json.broken")); err != nil {
		t.Fatal("the broken file is not kept aside")
	}
	s.Update("u", func(r *core.ReviewRequest) { r.State = "open" })
	again, _ := OpenReviewRequests(dir)
	if r, ok := again.Get("u"); !ok || r.State != "open" {
		t.Fatal("not saved")
	}
}
