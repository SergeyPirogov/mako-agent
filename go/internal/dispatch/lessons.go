package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// Lessons from review: when you correct the agent (in its chat, on the diff's lines, in the MR's comments), Mako
// draws a rule from it for the repo's CLAUDE.md, so the next agents know it too; it goes in only when you save it.

// LessonFrom draws the rule from your message; tests replace it.
var LessonFrom = writer.Lesson

// LessonsHeading: the section of CLAUDE.md the saved rules go under. A CLAUDE.md that already has the older
// Ukrainian one (LegacyLessonsHeading) keeps adding to that.
const (
	LessonsHeading       = "## Review lessons"
	LegacyLessonsHeading = "## Уроки з рев'ю" // cyrillic-ok: matches the heading in already-saved CLAUDE.md files
)

// A message this short is a «carry on» or a quick reply, not a correction.
const lessonMin = 15

// suggestLesson asks for a rule in what you told the task's agent and offers it on the board; one at a time per
// task, the newest replacing an unanswered one.
func (d *Dispatcher) suggestLesson(id, message string) {
	message = strings.TrimSpace(message)
	if utf8.RuneCountInString(message) < lessonMin {
		return
	}
	r, ok := d.Store.Get(id)
	if !ok || r.Worktree == "" || !exists(r.Worktree) {
		return
	}
	d.mu.Lock()
	if d.lessoning[id] {
		d.mu.Unlock()
		return
	}
	d.lessoning[id] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.lessoning, id)
		d.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	md, _ := os.ReadFile(filepath.Join(r.Worktree, "CLAUDE.md"))
	rule, err := LessonFrom(ctx, r.Worktree, message, string(md))
	if err != nil || rule == "" {
		return
	}
	d.Store.Update(id, func(r *core.Record) { r.Lesson = &core.Lesson{Rule: rule, From: clipText(message, 300)} })
	d.Log(id, "Mako suggests a lesson for CLAUDE.md: "+rule)
}

// SaveLesson answers the board's offer: "branch" commits the rule to CLAUDE.md in the task's branch, "base" to
// the base branch (pushed at once), "no" drops it. rule is the offer as you edited it.
func (d *Dispatcher) SaveLesson(ctx context.Context, id, to, rule string) error {
	r, ok := d.Store.Get(id)
	if !ok {
		return fmt.Errorf("no such task")
	}
	if to == "no" {
		d.Store.Update(id, func(r *core.Record) { r.Lesson = nil })
		return nil
	}
	rule = strings.Join(strings.Fields(rule), " ")
	if rule == "" {
		return fmt.Errorf("the rule is empty")
	}
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return fmt.Errorf("tracker %s was removed from settings", r.Task.TrackerID)
	}
	repo, err := d.route(t, r.Task)
	if err != nil {
		return err
	}
	msg := i18n.TA("commit.lesson", r.Task.ID, clipText(rule, 200))
	var branch, hash string
	switch to {
	case "branch":
		wt, _, err := d.workOf(id)
		if err != nil {
			return err
		}
		branch = branchOf(r)
		if hash, err = commitLesson(ctx, wt, rule, msg); err != nil {
			return err
		}
		d.Log(id, fmt.Sprintf("lesson written to CLAUDE.md on branch %s (goes into the MR with the next push): %s", branch, rule))
	case "base":
		if !exists(repo.Path) {
			return fmt.Errorf("repository %s does not exist", repo.Path)
		}
		branch = repo.BaseBranch
		if branch == "" {
			branch = gitx.DetectBaseBranch(repo.Path)
		}
		if hash, err = lessonToBase(ctx, repo.Path, branch, rule, msg); err != nil {
			return err
		}
		d.Log(id, fmt.Sprintf("lesson written to CLAUDE.md and pushed to %s: %s", branch, rule))
	default:
		return fmt.Errorf("unknown where to save: %q", to)
	}
	d.Store.Update(id, func(r *core.Record) { r.Lesson = nil })
	return d.keepLesson(core.SavedLesson{TrackerID: r.Task.TrackerID, Project: r.Task.Project, Repo: repo.Name, Rule: rule, Task: r.Task.ID,
		Branch: branch, Commit: hash, At: now()})
}

// lessonToBase commits the rule on top of origin's base branch in a worktree of its own (the clone's checkout
// is left as it is) and pushes it there.
func lessonToBase(ctx context.Context, repoPath, base, rule, msg string) (string, error) {
	if _, err := gitx.Run(ctx, repoPath, "fetch", "origin", base); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "mako-lesson-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	wt := filepath.Join(tmp, "wt")
	if _, err := gitx.Run(ctx, repoPath, "worktree", "add", "--detach", wt, "origin/"+base); err != nil {
		return "", err
	}
	defer gitx.Run(context.WithoutCancel(ctx), repoPath, "worktree", "remove", "--force", wt)
	hash, err := commitLesson(ctx, wt, rule, msg)
	if err != nil {
		return "", err
	}
	if _, err := gitx.Run(ctx, wt, "push", "origin", "HEAD:refs/heads/"+base); err != nil {
		return "", fmt.Errorf("could not push to %s (protected branch?): %w", base, err)
	}
	return hash, nil
}

// commitLesson adds the rule to CLAUDE.md at the worktree's root and commits that file alone: what else is
// staged there stays staged.
func commitLesson(ctx context.Context, wt, rule, msg string) (string, error) {
	path := filepath.Join(wt, "CLAUDE.md")
	md, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.WriteFile(path, []byte(AddRule(string(md), rule)), 0o644); err != nil {
		return "", err
	}
	if _, err := gitx.Run(ctx, wt, "add", "--", "CLAUDE.md"); err != nil {
		return "", err
	}
	if _, err := gitx.Run(ctx, wt, "commit", "-m", msg, "--", "CLAUDE.md"); err != nil {
		return "", err
	}
	hash, _ := gitx.Run(ctx, wt, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(hash), nil
}

// AddRule puts the rule last in CLAUDE.md's lessons section, making the section at the end when there is none.
func AddRule(md, rule string) string {
	item := "- " + rule
	body := strings.TrimRight(md, "\n")
	heading := LessonsHeading
	if strings.TrimSpace(body) == "" {
		return heading + "\n\n" + item + "\n"
	}
	lines := strings.Split(body, "\n")
	at := -1
	for i, l := range lines {
		if l = strings.TrimSpace(l); l == LessonsHeading || l == LegacyLessonsHeading {
			at = i
			break
		}
	}
	if at < 0 {
		return body + "\n\n" + heading + "\n\n" + item + "\n"
	}
	end := len(lines)
	for i := at + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "# ") || strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	ins := end
	for ins > at+1 && strings.TrimSpace(lines[ins-1]) == "" {
		ins--
	}
	add := []string{item}
	if ins == at+1 {
		add = []string{"", item}
	}
	if ins == end && end < len(lines) {
		add = append(add, "")
	}
	out := append(append(append([]string{}, lines[:ins]...), add...), lines[ins:]...)
	return strings.Join(out, "\n") + "\n"
}

func (d *Dispatcher) lessonsFile() string { return filepath.Join(d.Config().StateDir, "lessons.json") }

// Lessons: every rule saved to a CLAUDE.md, the newest first.
func (d *Dispatcher) Lessons() []core.SavedLesson {
	d.lessonsMu.Lock()
	defer d.lessonsMu.Unlock()
	return d.readLessons()
}

func (d *Dispatcher) readLessons() []core.SavedLesson {
	all := []core.SavedLesson{}
	if b, err := os.ReadFile(d.lessonsFile()); err == nil {
		json.Unmarshal(b, &all)
	}
	return all
}

func (d *Dispatcher) keepLesson(l core.SavedLesson) error {
	d.lessonsMu.Lock()
	defer d.lessonsMu.Unlock()
	all := append([]core.SavedLesson{l}, d.readLessons()...)
	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.MkdirAll(filepath.Dir(d.lessonsFile()), 0o755); err != nil {
		return err
	}
	tmp := d.lessonsFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.lessonsFile())
}
