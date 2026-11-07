package writer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

func TestParseIntake(t *testing.T) {
	for answer, want := range map[string]core.Intake{
		`{"decision":"take","why":"ясно"}`:                                           {State: "done", Decision: "take", Why: "ясно"},
		`Ось: {"decision":" Clarify ","why":"неясно","questions":["Що?"," ","Де?"]}`: {State: "done", Decision: "clarify", Why: "неясно", Questions: []string{"Що?", "Де?"}},
		`{"decision":"answer","why":"питання","answer":" Так, у web.go:1 "}`:         {State: "done", Decision: "answer", Why: "питання", Answer: "Так, у web.go:1"},
		`{"decision":"split","why":"три задачі","questions":["зайве"]}`:              {State: "done", Decision: "split", Why: "три задачі"},
	} {
		got, err := ParseIntake(answer)
		if err != nil || got.Decision != want.Decision || got.Why != want.Why || got.Answer != want.Answer || strings.Join(got.Questions, "|") != strings.Join(want.Questions, "|") {
			t.Errorf("%s: %+v, %v; want %+v", answer, got, err, want)
		}
	}
	for _, bad := range []string{
		`{"decision":"maybe"}`,
		`{"decision":"clarify","questions":[]}`,
		`{"decision":"answer","answer":"  "}`,
		`не знаю`,
	} {
		if got, err := ParseIntake(bad); err == nil {
			t.Errorf("%s: %+v, want an error", bad, got)
		}
	}
}

// The intake offers a split only when the tracker can open the subtasks, and a split it was not offered is an
// error; the prompt is in Mako's language.
func TestIntakeSplitOnlyWhenOffered(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\ncat > " + dir + "/prompt\necho '{\"decision\":\"split\",\"why\":\"дві задачі\"}'\n"
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755)
	old := Claude
	Claude = filepath.Join(dir, "claude")
	t.Cleanup(func() { Claude = old })
	defer i18n.SetAgentLanguage(i18n.AgentLanguage())
	task := core.Task{ID: "grp/app#5", Title: "Кнопка", Description: "Падає"}

	i18n.SetAgentLanguage(i18n.EN)
	if _, err := Intake(context.Background(), dir, "main", core.Route{Model: "haiku"}, task, false); err == nil {
		t.Fatal("a split the tracker cannot open was taken")
	}
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt"))
	if !strings.Contains(string(prompt), "## Intake: is there work to do") || strings.Contains(string(prompt), `"split": it is`) {
		t.Fatalf("prompt: %s", prompt)
	}

	i18n.SetAgentLanguage(i18n.UK)
	in, err := Intake(context.Background(), dir, "main", core.Route{Model: "haiku"}, task, true)
	if err != nil || in.Decision != core.IntakeSplit {
		t.Fatalf("%+v, %v", in, err)
	}
	prompt, _ = os.ReadFile(filepath.Join(dir, "prompt"))
	if !strings.Contains(string(prompt), "## Intake: чи є що робити") || !strings.Contains(string(prompt), `"split": це кілька задач`) || !strings.Contains(string(prompt), "origin/main") {
		t.Fatalf("prompt: %s", prompt)
	}
}
