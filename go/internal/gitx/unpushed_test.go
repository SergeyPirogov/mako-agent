package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A commit the remote branch lacks is unpushed work, unless the base branch already has the same change: the branch
// was rewritten after it was merged, so its commit got a new hash the remote branch never saw.
func TestUnpushedSkipsWhatTheBaseHas(t *testing.T) {
	wt, other, g := syncRepos(t)
	ctx := context.Background()
	if n := Unpushed(ctx, wt, "main"); n != 0 {
		t.Fatalf("pushed branch: %d unpushed", n)
	}

	// The branch's change lands in main (merged as a copy), and the branch is rewritten locally: a new hash, the
	// same change.
	g(other, "fetch", "-q", "origin", "fix/1")
	g(other, "cherry-pick", "FETCH_HEAD")
	g(other, "push", "-q", "origin", "main")
	g(wt, "fetch", "-q", "origin")
	g(wt, "commit", "-q", "--amend", "-m", "Add b, reworded")
	if n := Unpushed(ctx, wt, ""); n != 1 {
		t.Fatalf("without a base the rewritten commit is unpushed: %d", n)
	}
	if n := Unpushed(ctx, wt, "main"); n != 0 {
		t.Fatalf("the base has the change: %d unpushed", n)
	}

	// A change made after the merge is in neither: it is unpushed whatever the base.
	os.WriteFile(filepath.Join(wt, "lesson.md"), []byte("commit first\n"), 0o644)
	g(wt, "add", ".")
	g(wt, "commit", "-q", "-m", "Lesson")
	if n := Unpushed(ctx, wt, "main"); n != 1 {
		t.Fatalf("a change the base lacks: %d unpushed", n)
	}
	// A base that does not exist compares with the remote branch only.
	if n := Unpushed(ctx, wt, "no-such-base"); n != 2 {
		t.Fatalf("unknown base: %d unpushed", n)
	}
}
