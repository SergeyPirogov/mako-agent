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

// A review request that cannot be saved is told to OnSaveError.
func TestReviewRequestsSaveErrorIsTold(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	s, err := OpenReviewRequests(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Update("u", func(r *core.ReviewRequest) {})
	var told []error
	s.OnSaveError = func(err error) { told = append(told, err) }
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	s.Update("v", func(r *core.ReviewRequest) {})
	s.Delete("u")
	s.Delete("missing") // nothing to save: nothing to tell
	if len(told) != 2 {
		t.Fatalf("errors told = %v, want 2", told)
	}
}

// A second broken review-requests.json keeps the first copy set aside.
func TestBrokenReviewRequestsKeepsEarlierCopy(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "review-requests.json")
	os.WriteFile(file+".broken", []byte("{first"), 0o600)
	os.WriteFile(file, []byte("{second"), 0o600)
	if _, err := OpenReviewRequests(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file + ".broken"); string(b) != "{first" {
		t.Fatalf("the first copy = %q", b)
	}
	if more, _ := filepath.Glob(file + ".broken-*"); len(more) != 1 {
		t.Fatalf("second copies: %v", more)
	}
}
