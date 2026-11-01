package orca

import (
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// Every agent is told the language to write in, before how to finish.
func TestLanguageInPrompt(t *testing.T) {
	defer i18n.SetLang(i18n.Lang())
	j := Job{Task: core.Task{ID: "grp/app#5", Title: "Pay"}, Repo: &config.Repo{BaseBranch: "main"}, Branch: "feature/5"}
	for lang, want := range map[string]string{i18n.EN: "in English", i18n.UK: "in Ukrainian"} {
		i18n.SetLang(lang)
		p := FullPrompt(j, config.Orca{})
		if !strings.Contains(p, "## Language") || !strings.Contains(p, want) || !strings.Contains(p, "written in Russian") || !strings.Contains(p, "section headings named in English below") {
			t.Errorf("%s: no language note in\n%s", lang, p)
		}
		if strings.Index(p, "## Language") > strings.Index(p, "Finish with a merge request") {
			t.Errorf("%s: the language comes after how to finish", lang)
		}
	}
}
