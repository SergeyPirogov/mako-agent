package writer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// SpecTimeout: how long the agent may write the specification of an idea; it reads the code first.
const SpecTimeout = 10 * time.Minute

// specMax: the longest answer Spec takes: a specification and six tasks, in Ukrainian.
const specMax = 48000

// specQuestions, specTasks: the most questions and tasks an idea gets.
const specQuestions, specTasks = 5, 6

// SpecInput is the idea the agent writes a specification for: its text, its pictures (paths on this Mac) and, on
// the second run, the questions it asked with your answers.
type SpecInput struct {
	Text      string
	Images    []string
	Questions []core.SpecQuestion
	// Final: the questions were answered: it asks no more and gives the tasks.
	Final bool
}

// Spec: the agent, in the project's clone (dir) with tools that only read, writes the specification of an idea and
// either asks what it cannot tell or proposes the tasks to do it in. base: the branch the newest code is on,
// origin/<base>. The prompt is in Mako's language; the idea and the answers in it are data.
func Spec(ctx context.Context, dir, base string, route core.Route, in SpecInput) (core.SpecDraft, error) {
	mode := i18n.TA("agent.specAsk")
	if in.Final {
		mode = i18n.TA("agent.specFinal")
	}
	prompt := i18n.TA("agent.spec", base, mode) + "\n\n" + specData(in)
	var dirs []string
	for _, p := range in.Images {
		if d := filepath.Dir(p); !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	return askParsed(func() (string, error) {
		return runReadOnly(ctx, dir, prompt, route, SpecTimeout, specMax, dirs)
	}, func(s string) (core.SpecDraft, error) { return ParseSpec(s, in.Final) })
}

// specData: the idea, its pictures and the answers, as the prompt shows them: fenced as data.
func specData(in SpecInput) string {
	lines := []string{core.UntrustedNote, "", core.Untrusted("idea", false, clipTo(in.Text, 8000))}
	if len(in.Images) > 0 {
		lines = append(lines, "", i18n.TA("agent.specImages"))
		for _, p := range in.Images {
			lines = append(lines, chat.ImageLine(p))
		}
	}
	if len(in.Questions) > 0 {
		var qa []string
		for _, q := range in.Questions {
			a := strings.TrimSpace(q.A)
			if a == "" {
				a = i18n.TA("agent.specNoAnswer")
			}
			qa = append(qa, i18n.TA("agent.question")+" "+q.Q, i18n.TA("agent.assumption")+" "+orNone(q.Assume), "→ "+a, "")
		}
		lines = append(lines, "", i18n.TA("agent.specAnswers"), core.Untrusted("answers", false, strings.Join(qa, "\n")))
	}
	return strings.Join(lines, "\n")
}

// ParseSpec reads the JSON Spec asks for. A title and a specification are a must. Questions (at most five) come
// before tasks: with questions the tasks wait for the answers; final (the questions answered) takes no questions
// and needs the tasks. Tasks are read as a split's (ParseSplit), one to six of them.
func ParseSpec(answer string, final bool) (core.SpecDraft, error) {
	raw, err := jsonObject(answer, "spec")
	if err != nil {
		return core.SpecDraft{}, err
	}
	var in struct {
		Title, Spec string
		Questions   []struct{ Q, Assume string }
		Subtasks    []rawItem
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return core.SpecDraft{}, fmt.Errorf("%w: %v", errNotJSON, err)
	}
	d := core.SpecDraft{Title: clipTo(strings.TrimSpace(in.Title), 200), Body: clipTo(strings.TrimSpace(in.Spec), 30000)}
	if d.Title == "" || d.Body == "" {
		return core.SpecDraft{}, errors.New("the agent wrote no title or specification")
	}
	if !final {
		for _, q := range in.Questions {
			if text := strings.TrimSpace(q.Q); text != "" && len(d.Questions) < specQuestions {
				d.Questions = append(d.Questions, core.SpecQuestion{Q: clipTo(text, 500), Assume: clipTo(strings.TrimSpace(q.Assume), 500)})
			}
		}
		if len(d.Questions) > 0 {
			return d, nil
		}
	}
	d.Items = parseItems(in.Subtasks, specTasks)
	if len(d.Items) == 0 {
		return core.SpecDraft{}, errors.New("the agent proposed no tasks")
	}
	return d, nil
}
