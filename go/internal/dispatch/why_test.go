package dispatch

import (
	"context"
	"strings"
	"testing"
)

func TestWhyNot(t *testing.T) {
	d, f, _ := setup(t)
	ctx := context.Background()
	f.issues[6] = map[string]any{"id": 1006, "iid": 6, "state": "opened", "title": "Без мітки", "labels": []string{},
		"web_url": f.base + "/grp/app/-/issues/6", "assignees": []any{}, "references": map[string]string{"full": "grp/app#6"}}

	w, err := d.WhyNot(ctx, f.base+"/grp/app/-/issues/5")
	if err != nil || w.ID != "grp/app#5" || w.OnBoard || !strings.Contains(w.Verdict, "підходить") {
		t.Fatalf("a fitting task: %+v %v", w, err)
	}
	w, err = d.WhyNot(ctx, f.base+"/grp/app/-/issues/6")
	if err != nil || !strings.Contains(w.Verdict, "немає мітки «claude-agent»") {
		t.Fatalf("a task without the label: %+v %v", w, err)
	}
	d.SetPaused(true)
	w, _ = d.WhyNot(ctx, f.base+"/grp/app/-/issues/5")
	if !strings.Contains(w.Verdict, "на паузі") {
		t.Errorf("paused: %+v", w)
	}
	d.SetPaused(false)
	d.Poll(ctx)
	w, err = d.WhyNot(ctx, f.base+"/grp/app/-/issues/5")
	if err != nil || !w.OnBoard || !strings.Contains(w.Verdict, "«Нові»") {
		t.Errorf("a task on the board: %+v %v", w, err)
	}
	if _, err := d.WhyNot(ctx, "https://jira.example.com/browse/X-1"); err == nil || !strings.Contains(err.Error(), "жодного трекера") {
		t.Errorf("a foreign link: %v", err)
	}
}
