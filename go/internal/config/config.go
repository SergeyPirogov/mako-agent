// Package config reads the same mako.config.json and .env as the TypeScript Mako, keeping what the
// dispatcher uses: trackers and their projects, Orca, limits and notifications.
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	DefaultBranchPattern = "{type}/{id}-{title}"
	DefaultProjectsDir   = "~/mako/projects"
)

type Config struct {
	Path             string
	Home             string // the folder of mako.config.json and .env
	StateDir         string
	PollIntervalSec  int
	WipLimit         int
	ArchiveAfterDays int
	ProjectsDir      string
	BranchPattern    string
	Notifications    Notifications
	Orca             Orca
	Agent            Agent
	Comments         Comments
	Trackers         []*Tracker
	Env              map[string]string
}

// Agent: who works on a taken task and how. "mako": Mako runs Claude Code itself in the task's worktree and
// the developer talks to it from the board (Mac or phone); "orca": the task goes to Orca.
type Agent struct {
	Runner         string
	Command        string // Claude Code, "claude" by default
	Model          string
	PermissionMode string // "default", "acceptEdits" (default), "auto", "bypassPermissions"
	AllowedTools   []string
}

// DefaultAllowedTools: what the agent does without asking besides editing files — git and opening the MR.
var DefaultAllowedTools = []string{
	"Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git add:*)", "Bash(git commit:*)",
	"Bash(git push:*)", "Bash(glab mr create:*)", "Bash(gh pr create:*)",
}

// Comments Mako leaves in the issue, written as the developer: {branch} {taskId} {title} {url} {mr} are filled
// in; "-" leaves no comment.
type Comments struct {
	OnStart  string // when the task goes to Orca
	OnReview string // when its MR appears
}

const (
	DefaultCommentOnStart  = "Взяв задачу в роботу. Гілка {branch}. Як буде готово, відкрию MR."
	DefaultCommentOnReview = "Відкрив MR: {mr}"
)

type Notifications struct {
	Enabled   bool
	AutoTake  bool
	SnoozeMin int
	Port      int
}

// Orca is the executor the dispatcher hands every task to.
type Orca struct {
	Command      string
	Agent        string
	Setup        string // "run", "skip", "inherit"
	LinkIssue    bool
	Instructions string // replaces the default end of the prompt; {branch} {baseBranch} {taskId} {title} {url} {closingRef}
	MRBy         string // "agent" (default) or "me"
	// SummaryComment: when the MR appears, the summary of the agent's work also goes to the issue as a comment.
	SummaryComment bool
	ExtraArgs      []string
}

type Transitions struct {
	OnStart, OnSuccess, OnFailure, OnMerged *string // nil: not set (Mako guesses where it can); "": leave alone
}

type Tracker struct {
	Type            string // "gitlab", "github", "jira"
	ID              string
	BaseURL         string // GitLab, Jira
	APIURL          string // GitHub Enterprise
	JQL             string
	Trigger         string // the label a task needs; "" with TriggerOff: any of my tasks
	TriggerOff      bool
	Assignee        string
	AllProjects     bool
	AssignOnTake    bool
	SkipOthersTasks bool
	Transitions     Transitions
	Projects        map[string]*Project
}

type Project struct {
	Key   string
	Repos map[string]*Repo
	// Default repo when a project has several; with one it is that one.
	Default string
}

type Repo struct {
	Name          string
	Path          string
	CloneURL      string
	BaseBranch    string // "" until known: the remote's default
	BranchPattern string
	WorktreesDir  string
}

// ErrMissing: there is no mako.config.json yet (first start).
var ErrMissing = errors.New("Mako ще не налаштований")

// Load reads mako.config.json and .env from home. When the file is missing or wrong it still returns a config
// with the defaults, so Mako starts and the settings page can fix it; the error says what is wrong.
func Load(home string) (*Config, error) {
	path := filepath.Join(home, "mako.config.json")
	env := readEnv(filepath.Join(home, ".env"))
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		err = ErrMissing
	}
	var c *Config
	if err == nil {
		c, err = Parse(data, env)
	}
	if err != nil {
		c, _ = Parse([]byte("{}"), env)
	}
	c.Path, c.Home = path, home
	// As the TypeScript Mako: .mako next to the config, a relative stateDir from there too.
	if c.StateDir == "" {
		c.StateDir = ".mako"
	}
	if !filepath.IsAbs(c.StateDir) {
		c.StateDir = filepath.Join(home, c.StateDir)
	}
	return c, err
}

// SaveEnv sets keys in home/.env, keeping its other lines; an empty value removes the key.
func SaveEnv(home string, updates map[string]string) error {
	path := filepath.Join(home, ".env")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var lines []string
	done := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		k, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		k = strings.TrimSpace(k)
		if v, change := updates[k]; ok && change && !strings.HasPrefix(k, "#") {
			done[k] = true
			if v == "" {
				continue
			}
			line = k + "=" + v
		}
		if line != "" || len(lines) > 0 {
			lines = append(lines, line)
		}
	}
	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !done[k] && updates[k] != "" {
			lines = append(lines, k+"="+updates[k])
		}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// Parse turns the raw config into what the dispatcher needs and fills the defaults.
func Parse(data []byte, env map[string]string) (*Config, error) {
	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("mako.config.json: %w", err)
	}
	c := &Config{
		StateDir:         Expand(raw.StateDir),
		PollIntervalSec:  orInt(raw.PollIntervalSec, 60),
		WipLimit:         orInt(raw.WipLimit, 0),
		ArchiveAfterDays: orInt(raw.ArchiveAfterDays, 2),
		ProjectsDir:      Expand(orStr(raw.ProjectsDir, DefaultProjectsDir)),
		BranchPattern:    orStr(raw.BranchPattern, DefaultBranchPattern),
		Env:              env,
		Notifications: Notifications{
			Enabled:   raw.Notifications.Enabled == nil || *raw.Notifications.Enabled,
			AutoTake:  raw.Notifications.AutoTake != nil && *raw.Notifications.AutoTake,
			SnoozeMin: orInt(raw.Notifications.SnoozeMin, 60),
			Port:      orInt(raw.Notifications.InboxPort, 7777),
		},
		Orca: Orca{Agent: "claude", Setup: "inherit", LinkIssue: true, MRBy: "agent"},
		Agent: Agent{
			Runner:         orStr(raw.Agent.Runner, "mako"),
			Command:        raw.Agent.Command,
			Model:          raw.Agent.Model,
			PermissionMode: orStr(raw.Agent.PermissionMode, "acceptEdits"),
			AllowedTools:   raw.Agent.AllowedTools,
		},
		Comments: Comments{
			OnStart:  orStr(strings.TrimSpace(raw.Comments.OnStart), DefaultCommentOnStart),
			OnReview: orStr(strings.TrimSpace(raw.Comments.OnReview), DefaultCommentOnReview),
		},
	}
	// The dispatcher's Orca: the first orca executor, else the defaults.
	for _, name := range raw.executorOrder() {
		e := raw.Executors[name]
		if e.Type != "orca" {
			continue
		}
		c.Orca.Command = e.Command
		c.Orca.Agent = orStr(e.Agent, "claude")
		c.Orca.Setup = orStr(e.Setup, "inherit")
		c.Orca.LinkIssue = e.LinkIssue == nil || *e.LinkIssue
		c.Orca.Instructions = e.Instructions
		c.Orca.MRBy = orStr(e.MRBy, "agent")
		c.Orca.SummaryComment = e.SummaryComment
		c.Orca.ExtraArgs = e.ExtraArgs
		break
	}
	for i, rt := range raw.Trackers {
		t, err := c.tracker(rt)
		if err != nil {
			return nil, fmt.Errorf("трекер %d: %w", i+1, err)
		}
		c.Trackers = append(c.Trackers, t)
	}
	if c.Agent.AllowedTools == nil {
		c.Agent.AllowedTools = DefaultAllowedTools
	}
	if c.Agent.Runner != "orca" {
		c.Agent.Runner = "mako"
	}
	return c, nil
}

func (c *Config) tracker(rt rawTracker) (*Tracker, error) {
	t := &Tracker{
		Type:            rt.Type,
		ID:              orStr(rt.ID, rt.Type),
		BaseURL:         strings.TrimRight(rt.BaseURL, "/"),
		APIURL:          strings.TrimRight(rt.APIURL, "/"),
		JQL:             rt.JQL,
		Assignee:        strings.TrimPrefix(strings.TrimSpace(rt.Assignee), "@"),
		AllProjects:     rt.AllProjects,
		AssignOnTake:    rt.AssignOnTake == nil || *rt.AssignOnTake,
		SkipOthersTasks: rt.SkipOthersTasks == nil || *rt.SkipOthersTasks,
		Projects:        map[string]*Project{},
	}
	switch t.Type {
	case "gitlab", "github", "jira":
	default:
		return nil, fmt.Errorf("невідомий тип трекера %q", t.Type)
	}
	// labels.trigger: missing → "claude-agent"; null → no label needed (all of my tasks).
	if rt.Labels != nil {
		if raw, ok := rt.Labels["trigger"]; ok {
			if string(raw) == "null" {
				t.TriggerOff = true
			} else {
				_ = json.Unmarshal(raw, &t.Trigger)
			}
		}
	}
	if !t.TriggerOff && t.Trigger == "" && t.Type != "jira" {
		t.Trigger = "claude-agent"
	}
	if t.Type == "gitlab" && t.BaseURL == "" {
		t.BaseURL = "https://gitlab.com"
	}
	if t.Type == "jira" && t.BaseURL == "" {
		t.BaseURL = strings.TrimRight(c.Env["JIRA_BASE_URL"], "/")
	}
	t.Transitions = Transitions{
		OnStart: transition(rt.Transitions, "onStart"), OnSuccess: transition(rt.Transitions, "onSuccess"),
		OnFailure: transition(rt.Transitions, "onFailure"), OnMerged: transition(rt.Transitions, "onMerged"),
	}
	for key, rp := range rt.Projects {
		var p rawProject
		if err := json.Unmarshal(rp, &p); err != nil {
			return nil, fmt.Errorf("проєкт %s: %w", key, err)
		}
		project := &Project{Key: key, Repos: map[string]*Repo{}, Default: p.Default}
		repos := p.Repos
		several := len(repos) > 1
		if len(repos) == 0 {
			// Short form: the project itself is its one repo.
			var own rawRepo
			_ = json.Unmarshal(rp, &own)
			repos = map[string]rawRepo{lastPart(key): own}
		}
		for name, r := range repos {
			clone := ""
			if !several {
				clone = DefaultCloneURL(t, key)
			}
			project.Repos[name] = c.NewRepo(name, r.RepoPath, orStr(r.CloneURL, clone), r.BaseBranch, r.BranchPattern)
		}
		t.Projects[key] = project
	}
	if len(t.Projects) == 0 && !t.AllProjects {
		return nil, fmt.Errorf("%s: додайте проєкт або увімкніть «Мої задачі з усіх проєктів»", t.ID)
	}
	return t, nil
}

// NewRepo is one local repository with the defaults: ~/mako/projects/<name>, the general branch pattern.
func (c *Config) NewRepo(name, path, cloneURL, base, pattern string) *Repo {
	p := Expand(path)
	if p == "" {
		p = filepath.Join(c.ProjectsDir, name)
	}
	return &Repo{
		Name:          name,
		Path:          p,
		CloneURL:      cloneURL,
		BaseBranch:    base,
		BranchPattern: orStr(pattern, c.BranchPattern),
		WorktreesDir:  filepath.Join(filepath.Dir(p), ".worktrees", name),
	}
}

// DefaultCloneURL is the SSH address of a GitLab/GitHub project ("group/app"), the way developers clone.
func DefaultCloneURL(t *Tracker, project string) string {
	if !strings.Contains(project, "/") {
		return "" // a Jira key says nothing about where the code is
	}
	host := ""
	switch t.Type {
	case "gitlab":
		if u, err := url.Parse(t.BaseURL); err == nil {
			host = u.Hostname()
		}
	case "github":
		host = "github.com"
		if t.APIURL != "" {
			if u, err := url.Parse(t.APIURL); err == nil {
				host = strings.TrimPrefix(u.Hostname(), "api.")
			}
		}
	}
	if host == "" {
		return ""
	}
	return fmt.Sprintf("git@%s:%s.git", host, project)
}

// Token of a tracker from the environment (.env, then the process).
func (c *Config) Get(name string) string {
	if v := c.Env[name]; v != "" {
		return v
	}
	return os.Getenv(name)
}

// Expand turns a leading ~/ into the home folder.
func Expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func readEnv(path string) map[string]string {
	env := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return env
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return env
}

// transition: absent → nil (Mako guesses where it can), null → "" (leave the status alone), a name → it.
func transition(m map[string]json.RawMessage, key string) *string {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	name := ""
	if string(raw) != "null" {
		_ = json.Unmarshal(raw, &name)
	}
	return &name
}

func lastPart(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return key
}

func orStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}

func orInt(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

type rawConfig struct {
	StateDir         string                 `json:"stateDir"`
	PollIntervalSec  *int                   `json:"pollIntervalSec"`
	WipLimit         *int                   `json:"wipLimit"`
	ArchiveAfterDays *int                   `json:"archiveAfterDays"`
	ProjectsDir      string                 `json:"projectsDir"`
	BranchPattern    string                 `json:"branchPattern"`
	Notifications    rawNotifications       `json:"notifications"`
	Executors        map[string]rawExecutor `json:"executors"`
	Agent            struct {
		Runner         string   `json:"runner"`
		Command        string   `json:"command"`
		Model          string   `json:"model"`
		PermissionMode string   `json:"permissionMode"`
		AllowedTools   []string `json:"allowedTools"`
	} `json:"agent"`
	Comments struct {
		OnStart  string `json:"onStart"`
		OnReview string `json:"onReview"`
	} `json:"comments"`
	DefaultExecutor string       `json:"defaultExecutor"`
	Trackers        []rawTracker `json:"trackers"`
}

// executorOrder: the default executor first, so a config with several Orcas uses the chosen one.
func (r rawConfig) executorOrder() []string {
	names := []string{}
	if _, ok := r.Executors[r.DefaultExecutor]; ok {
		names = append(names, r.DefaultExecutor)
	}
	for n := range r.Executors {
		if n != r.DefaultExecutor {
			names = append(names, n)
		}
	}
	return names
}

type rawNotifications struct {
	Enabled   *bool `json:"enabled"`
	AutoTake  *bool `json:"autoTake"`
	SnoozeMin *int  `json:"snoozeMin"`
	InboxPort *int  `json:"inboxPort"`
}

type rawExecutor struct {
	Type           string   `json:"type"`
	Command        string   `json:"command"`
	Agent          string   `json:"agent"`
	Setup          string   `json:"setup"`
	LinkIssue      *bool    `json:"linkIssue"`
	Instructions   string   `json:"instructions"`
	MRBy           string   `json:"mrBy"`
	SummaryComment bool     `json:"summaryComment"`
	ExtraArgs      []string `json:"extraArgs"`
}

type rawTracker struct {
	Type            string                     `json:"type"`
	ID              string                     `json:"id"`
	BaseURL         string                     `json:"baseUrl"`
	APIURL          string                     `json:"apiUrl"`
	JQL             string                     `json:"jql"`
	Labels          map[string]json.RawMessage `json:"labels"`
	Assignee        string                     `json:"assignee"`
	AllProjects     bool                       `json:"allProjects"`
	AssignOnTake    *bool                      `json:"assignOnTake"`
	SkipOthersTasks *bool                      `json:"skipOthersTasks"`
	Transitions     map[string]json.RawMessage `json:"transitions"`
	Projects        map[string]json.RawMessage `json:"projects"`
}

type rawProject struct {
	Default string             `json:"default"`
	Repos   map[string]rawRepo `json:"repos"`
}

type rawRepo struct {
	RepoPath      string `json:"repoPath"`
	CloneURL      string `json:"cloneUrl"`
	BaseBranch    string `json:"baseBranch"`
	BranchPattern string `json:"branchPattern"`
}
