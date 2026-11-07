package writer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// SplitTimeout: how long the split of a task may take.
const SplitTimeout = 5 * time.Minute

// splitMax: the longest answer Split takes: six subtasks, each with a description in Ukrainian.
const splitMax = 16000

// Split: the agent's proposal to split a big task into 2–6 subtasks, each its own MR, in the order they are to be
// done, with the earlier ones each waits for. From the task's text and the repository's layout; in Mako's language.
func Split(ctx context.Context, dir string, route core.Route, t core.Task, outline string) ([]core.SplitItem, error) {
	prompt := strings.Join([]string{
		"The task below is too big for one change. Split it into 2 to 6 subtasks for a coding agent: each one a change",
		"that can be reviewed and merged on its own and leaves the code working, together doing all the task asks and",
		"nothing more. List them in the order they are to be done. You see the task's text and the repository's layout,",
		"not the code. Answer with JSON only, no code fences, in this shape:",
		`{"subtasks":[{"title":"...","description":"...","after":[]}]}`,
		"- title: what the subtask does, one short line;",
		"- description: what to change and how to tell it is done (tests included), at most 600 characters, Markdown allowed;",
		"- after: the numbers (from 1) of the subtasks above it that must be merged before it starts; [] when it can start at once.",
		"Title and description in " + lang() + "; paths, names and commands stay as they are.",
		"", taskText(t, 8000, true),
		"", "Repository:", orNone(outline),
	}, "\n")
	return askParsed(func() (string, error) { return askModelUpTo(ctx, dir, route, prompt, SplitTimeout, splitMax) }, ParseSplit)
}

// ParseSplit reads the JSON Split asks for. A subtask without a title is left out and the numbers of its after
// move with it; an after that is not of a subtask above is dropped; past six the rest are cut. Fewer than two
// left is an error: that is no split.
func ParseSplit(answer string) ([]core.SplitItem, error) {
	raw, err := jsonObject(answer, "subtasks")
	if err != nil {
		return nil, err
	}
	var in struct{ Subtasks []rawItem }
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("%w: %v", errNotJSON, err)
	}
	out := parseItems(in.Subtasks, 6)
	if len(out) < 2 {
		return nil, errors.New("the agent did not split the task into two or more subtasks")
	}
	return out, nil
}

// rawItem is a subtask as the model writes it.
type rawItem struct {
	Title, Description string
	After              []int
}

// parseItems: the subtasks as the board edits them, at most most of them. One without a title is left out and the
// numbers of its after move with it; an after that is not of a subtask above is dropped.
func parseItems(in []rawItem, most int) []core.SplitItem {
	var out []core.SplitItem
	number := map[int]int{} // the number in the answer → the number in out
	for i, s := range in {
		title := strings.TrimSpace(s.Title)
		if title == "" {
			continue
		}
		if len(out) == most {
			break
		}
		item := core.SplitItem{Title: clipTo(title, 200), Description: clipTo(strings.TrimSpace(s.Description), 2000)}
		for _, a := range s.After {
			if n, ok := number[a]; ok && a < i+1 && !slices.Contains(item.After, n) {
				item.After = append(item.After, n)
			}
		}
		out = append(out, item)
		number[i+1] = len(out)
	}
	return out
}
