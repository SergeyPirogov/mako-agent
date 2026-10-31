package orca

import (
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// UI tasks get «before» and «after» screenshots: from the base branch and from the worktree.
func TestScreenshotsInPrompt(t *testing.T) {
	j := Job{Task: core.Task{ID: "grp/app#5", Title: "Pay"}, Repo: &config.Repo{BaseBranch: "develop"}, Branch: "feature/5"}
	if strings.Contains(FullPrompt(j, config.Orca{}), "Screenshots") {
		t.Fatal("screenshots without a Mako to take them")
	}
	defer func(old string) { MakoCommand = old }(MakoCommand)
	MakoCommand = ShellQuote("/Applications/Mako app/mako")
	p := FullPrompt(j, config.Orca{})
	for _, want := range []string{
		"'/Applications/Mako app/mako' screenshot -task 'grp/app#5' -name before -rev origin/develop",
		"-task 'grp/app#5' -name after`",
		"## Screenshots",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("no %q in\n%s", want, p)
		}
	}
	if strings.Index(p, "Screenshots for UI") > strings.Index(p, "Finish with a merge request") {
		t.Error("screenshots come after how to finish")
	}
}
