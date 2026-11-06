package writer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestParseSplit(t *testing.T) {
	got, err := ParseSplit("Here it is: " + `{"subtasks":[
		{"title":" Model ","description":"the types","after":[]},
		{"title":"","description":"no title: left out","after":[1]},
		{"title":"API","description":"handlers","after":[1,1,2,4]},
		{"title":"Board","description":"the card","after":[3,1]}
	]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.SplitItem{
		{Title: "Model", Description: "the types"},
		// 2 was left out, 4 is below it: only 1 stays, once.
		{Title: "API", Description: "handlers", After: []int{1}},
		// 3 of the answer is 2 now.
		{Title: "Board", Description: "the card", After: []int{2, 1}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}

	var many []string
	for range 8 {
		many = append(many, `{"title":"x"}`)
	}
	if got, err := ParseSplit(`{"subtasks":[` + strings.Join(many, ",") + `]}`); err != nil || len(got) != 6 {
		t.Fatalf("eight subtasks: %d, %v; want six", len(got), err)
	}
	if _, err := ParseSplit(`{"subtasks":[{"title":"one"}]}`); err == nil {
		t.Fatal("one subtask is no split")
	}
	if _, err := ParseSplit("I cannot split it"); err == nil {
		t.Fatal("an answer with no JSON is an error")
	}
}

// A list of six subtasks in Ukrainian is longer than the 6000 bytes of an ordinary answer: Split takes it.
func TestSplitTakesLongAnswer(t *testing.T) {
	var subs []map[string]any
	for range 6 {
		subs = append(subs, map[string]any{"title": "Підзадача", "description": strings.Repeat("опис ", 120), "after": []int{}})
	}
	answer, _ := json.Marshal(map[string]any{"subtasks": subs})
	if len(answer) <= answerMax {
		t.Fatalf("the answer is %d bytes: not longer than an ordinary one", len(answer))
	}
	line, _ := json.Marshal(map[string]any{"type": "result", "result": string(answer), "total_cost_usd": 0})
	out := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(out, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeClaude(t, "cat > /dev/null; cat "+out)
	got, err := Split(context.Background(), t.TempDir(), core.Route{}, core.Task{ID: "g/a#1", Title: "Big"}, "")
	if err != nil || len(got) != 6 {
		t.Fatalf("got %d subtasks, %v", len(got), err)
	}
	if _, err := AskModel(context.Background(), t.TempDir(), core.Route{}, "x", SplitTimeout); err == nil {
		t.Fatal("an ordinary ask still takes no answer that long")
	}
}
