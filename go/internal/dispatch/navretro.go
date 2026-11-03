package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/navstat"
	"github.com/SergeyPirogov/mako-agent/go/internal/orca"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// The navigation retro («Ретро навігації»): Mako reads the transcripts of a repository's last merged tasks, counts
// where their agents searched long, read the same files again, searched in vain or read docs before editing
// elsewhere (navstat), and asks the model what would make the repository easier to navigate. The suggestions show
// on the board with their evidence: a line for CLAUDE.md is saved under its own heading, the rest become tasks.

// NavRetroTasks: how many of a repository's last merged tasks the navigation retro reads.
const NavRetroTasks = 10

// NavHeading: the section of CLAUDE.md the navigation retro's lines go under.
const NavHeading = "## Navigation"

// navRetroTimeout bounds a run: the transcripts and the model, asked twice when its first answer is no JSON.
const navRetroTimeout = 9 * time.Minute

// NavFrom asks the model for the suggestions; tests replace it.
var NavFrom = writer.NavRetro

// NavRetroState is a repository's navigation retro as the board shows it: the last one (State "" when none was
// made yet) and how many merged tasks it can read.
type NavRetroState struct {
	core.NavRetro
	Key    string `json:"key"`
	Merged int    `json:"merged"`
}

func navKey(tracker, project, repo string) string { return tracker + "/" + project + "/" + repo }

func (d *Dispatcher) navFile() string { return filepath.Join(d.Config().StateDir, "navretro.json") }

// readNav: the retros kept, by navKey; the caller holds navMu.
func (d *Dispatcher) readNav() map[string]core.NavRetro {
	all := map[string]core.NavRetro{}
	if b, err := os.ReadFile(d.navFile()); err == nil {
		json.Unmarshal(b, &all)
	}
	return all
}

// updateNav changes the retro of key in navretro.json; change returns an error to leave the file as it is.
func (d *Dispatcher) updateNav(key string, change func(x *core.NavRetro) error) (core.NavRetro, error) {
	d.navMu.Lock()
	defer d.navMu.Unlock()
	all := d.readNav()
	x := all[key]
	if err := change(&x); err != nil {
		return x, err
	}
	all[key] = x
	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.MkdirAll(filepath.Dir(d.navFile()), 0o755); err != nil {
		return x, err
	}
	return x, fsx.WriteFile(d.navFile(), b, 0o600)
}

// navRepo: the merged tasks of a repository, the newest first, and the repository itself.
type navRepo struct {
	tracker, project string
	repo             *config.Repo
	records          []core.Record
}

// navRepos: the repositories of project that have merged tasks, by navKey.
func (d *Dispatcher) navRepos(project string) map[string]*navRepo {
	out := map[string]*navRepo{}
	for _, r := range d.Store.All() {
		if r.ReviewState != "merged" || r.Task.Project != project {
			continue
		}
		repo := d.repoOf(r)
		if repo == nil {
			continue
		}
		key := navKey(r.Task.TrackerID, project, repo.Name)
		if out[key] == nil {
			out[key] = &navRepo{tracker: r.Task.TrackerID, project: project, repo: repo}
		}
		out[key].records = append(out[key].records, r)
	}
	for _, x := range out {
		sort.SliceStable(x.records, func(i, j int) bool { return x.records[i].MergedAt > x.records[j].MergedAt })
	}
	return out
}

// navRepoOf: the repository of key, from the project's merged tasks.
func (d *Dispatcher) navRepoOf(key string) (*navRepo, error) {
	x := d.readNavLocked(key)
	project := x.Project
	if project == "" {
		// Not run yet: the project is what is between the tracker and the repo in the key.
		for _, r := range d.Store.All() {
			if r.ReviewState == "merged" && strings.HasPrefix(key, r.Task.TrackerID+"/"+r.Task.Project+"/") {
				project = r.Task.Project
				break
			}
		}
	}
	if repo := d.navRepos(project)[key]; repo != nil {
		return repo, nil
	}
	return nil, errors.New("the repository has no merged tasks")
}

func (d *Dispatcher) readNavLocked(key string) core.NavRetro {
	d.navMu.Lock()
	defer d.navMu.Unlock()
	return d.readNav()[key]
}

// NavRetros: the navigation retros of project's repositories with merged tasks, for the board.
func (d *Dispatcher) NavRetros(project string) []NavRetroState {
	if project == "" {
		return nil
	}
	repos := d.navRepos(project)
	d.navMu.Lock()
	kept := d.readNav()
	d.navMu.Unlock()
	out := []NavRetroState{}
	for key, x := range repos {
		s := NavRetroState{NavRetro: kept[key], Key: key, Merged: min(len(x.records), NavRetroTasks)}
		s.TrackerID, s.Project, s.Repo = x.tracker, project, x.repo.Name
		if s.State == "running" && navStale(s.At) {
			s.State, s.Error = "failed", "the retro stopped without an answer"
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

// navStale: a run started this long ago is over, whatever it says.
func navStale(at string) bool {
	t, err := time.Parse(time.RFC3339Nano, at)
	return err != nil || time.Since(t) > navRetroTimeout+time.Minute
}

// NavRetro starts the navigation retro of the repository key (tracker/project/repo) in the background.
func (d *Dispatcher) NavRetro(key string) error {
	repo, err := d.startNavRetro(key)
	if err != nil {
		return err
	}
	go d.navRetro(context.Background(), key, repo)
	return nil
}

// startNavRetro marks the retro as running, unless one already runs or there is nothing to read.
func (d *Dispatcher) startNavRetro(key string) (*navRepo, error) {
	repo, err := d.navRepoOf(key)
	if err != nil {
		return nil, err
	}
	_, err = d.updateNav(key, func(x *core.NavRetro) error {
		if x.State == "running" && !navStale(x.At) {
			return errors.New("the navigation retro is already running")
		}
		*x = core.NavRetro{TrackerID: repo.tracker, Project: repo.project, Repo: repo.repo.Name, At: now(), State: "running"}
		return nil
	})
	return repo, err
}

// navRetro reads the transcripts, asks the model and keeps what it suggests.
func (d *Dispatcher) navRetro(ctx context.Context, key string, repo *navRepo) {
	ctx, cancel := context.WithTimeout(ctx, navRetroTimeout)
	defer cancel()
	var tasks []navstat.Task
	var read, skipped []string
	for _, r := range repo.records {
		if len(tasks) == NavRetroTasks {
			break
		}
		path := transcriptOf(r)
		if path == "" {
			d.Log(r.ID, "navigation retro: the agent's transcript is not kept, the task is skipped")
			skipped = append(skipped, r.ID)
			continue
		}
		s, ok := navstat.AnalyzeFile(path, r.Worktree)
		if !ok {
			d.Log(r.ID, "navigation retro: the agent's transcript cannot be read, the task is skipped")
			skipped = append(skipped, r.ID)
			continue
		}
		tasks = append(tasks, navstat.Task{ID: r.ID, Title: r.Task.Title, Session: s})
		read = append(read, r.ID)
	}
	var suggestions []core.NavSuggestion
	err := errors.New("none of the last merged tasks has its agent's transcript")
	if len(tasks) > 0 {
		dir := repo.repo.Path
		if !exists(dir) {
			dir = os.TempDir()
		}
		route := routeFor(d.Config(), core.Record{}, core.PhaseAssess, repo.repo.RouteMode)
		suggestions, err = NavFrom(ctx, dir, route, navstat.Digest(tasks), claudeMDOf(ctx, repo.repo))
	}
	x := core.NavRetro{TrackerID: repo.tracker, Project: repo.project, Repo: repo.repo.Name, At: now(), State: "done",
		Tasks: read, Skipped: skipped, Suggestions: suggestions}
	if err != nil {
		x.State, x.Error, x.Suggestions = "failed", err.Error(), nil
		d.Log(key, fmt.Sprintf("the navigation retro of %s failed: %v", repo.repo.Name, err))
	} else {
		d.Log(key, fmt.Sprintf("the navigation retro of %s read %d tasks and suggests %d changes", repo.repo.Name, len(read), len(suggestions)))
	}
	d.updateNav(key, func(old *core.NavRetro) error { *old = x; return nil })
}

// transcriptOf: the transcript of the task's agent: Mako's own agent's session, or, for an agent without one Mako
// knows (in Orca), the newest session in its worktree; "" when none is kept. Claude Code keeps it after the
// worktree is removed.
func transcriptOf(r core.Record) string {
	if r.Worktree == "" {
		return ""
	}
	if r.SessionID != "" {
		return orca.SessionFile(r.Worktree, r.SessionID)
	}
	return orca.LatestTranscript(r.Worktree)
}

// claudeMDOf: the repository's CLAUDE.md as it is on origin's base branch, else in the clone.
func claudeMDOf(ctx context.Context, repo *config.Repo) string {
	if !exists(repo.Path) {
		return ""
	}
	base := repo.BaseBranch
	if base == "" {
		base = gitx.DetectBaseBranch(repo.Path)
	}
	if md, err := gitx.Run(ctx, repo.Path, "show", "origin/"+base+":CLAUDE.md"); err == nil {
		return md
	}
	b, _ := os.ReadFile(filepath.Join(repo.Path, "CLAUDE.md"))
	return string(b)
}

// SaveNavRule: «Зберегти в CLAUDE.md» for a suggestion of the retro of key: the rule (as you edited it) goes under
// NavHeading in CLAUDE.md on top of origin's base branch, pushed at once, and into the saved lessons.
func (d *Dispatcher) SaveNavRule(ctx context.Context, key, id, rule string) error {
	rule = strings.Join(strings.Fields(rule), " ")
	if rule == "" {
		return errors.New("the rule is empty")
	}
	x := d.readNavLocked(key)
	i := slices.IndexFunc(x.Suggestions, func(s core.NavSuggestion) bool { return s.ID == id })
	if i < 0 {
		return errors.New("no such suggestion")
	}
	if x.Suggestions[i].Kind != core.NavClaudeMD {
		return errors.New("only a line for CLAUDE.md is saved there")
	}
	repo, err := d.navRepoOf(key)
	if err != nil {
		return err
	}
	if !exists(repo.repo.Path) {
		return fmt.Errorf("repository %s does not exist", repo.repo.Path)
	}
	branch := repo.repo.BaseBranch
	if branch == "" {
		branch = gitx.DetectBaseBranch(repo.repo.Path)
	}
	hash, err := lessonToBase(ctx, repo.repo.Path, branch, NavHeading, rule, i18n.TA("commit.navrule", clipText(rule, 200)))
	if err != nil {
		return err
	}
	d.Log(key, fmt.Sprintf("navigation hint written to CLAUDE.md of %s and pushed to %s: %s", repo.repo.Name, branch, rule))
	if err := d.markNav(key, id, "saved", rule); err != nil {
		return err
	}
	return d.keepLesson(core.SavedLesson{TrackerID: repo.tracker, Project: repo.project, Repo: repo.repo.Name, Rule: rule,
		Branch: branch, Commit: hash, At: now(), Section: "navigation"})
}

// MarkNav answers a suggestion of the retro of key: "no" drops it, "task:<id>" tells the task made of it.
func (d *Dispatcher) MarkNav(key, id, done string) error {
	if done != "no" && (!strings.HasPrefix(done, "task:") || len(done) == len("task:")) {
		return fmt.Errorf("unknown answer %q", done)
	}
	return d.markNav(key, id, done, "")
}

func (d *Dispatcher) markNav(key, id, done, rule string) error {
	_, err := d.updateNav(key, func(x *core.NavRetro) error {
		i := slices.IndexFunc(x.Suggestions, func(s core.NavSuggestion) bool { return s.ID == id })
		if i < 0 {
			return errors.New("no such suggestion")
		}
		x.Suggestions[i].Done = done
		if rule != "" {
			x.Suggestions[i].Rule = rule
		}
		return nil
	})
	return err
}
