// Package gitx is what Mako needs from git: branch names, clones and the remote's default branch.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "h", 'ґ': "g", 'д': "d", 'е': "e", 'є': "ye", 'ж': "zh", 'з': "z", 'и': "y", 'і': "i",
	'ї': "yi", 'й': "i", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch", 'ь': "", 'ю': "yu", 'я': "ya", 'ы': "y", 'э': "e",
	'ё': "yo", 'ъ': "",
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

// SlugifyTitle: «Розібрати нічний ран» → "rozibraty-nichnyi-ran", at most max characters, whole words.
func SlugifyTitle(title string, max int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		if t, ok := translit[r]; ok {
			b.WriteString(t)
		} else {
			b.WriteRune(r)
		}
	}
	out := ""
	for _, w := range strings.Fields(nonWord.ReplaceAllString(b.String(), " ")) {
		if out != "" && len(out)+1+len(w) > max {
			break
		}
		if out == "" {
			if len(w) > max {
				w = w[:max]
			}
			out = w
		} else {
			out += "-" + w
		}
	}
	if out == "" {
		return "task"
	}
	return out
}

var (
	bugWord     = regexp.MustCompile(`\b(bug|fix|defect|hotfix|incident)\b|баг|помилка`)
	featureWord = regexp.MustCompile(`\b(feature|enhancement)\b|фіча`)
	titleBug    = regexp.MustCompile(`(?i)^\s*(fix|bug)`)
)

// TaskType: fix for a bug, feature for a task. A GitLab Task is a feature; a GitLab Issue or Incident is a bug
// unless labelled feature/enhancement; elsewhere a bug label, Jira's Bug or a title starting with fix/bug.
func TaskType(t core.Task) string {
	meta := strings.ToLower(t.Meta)
	labels := strings.ToLower(strings.Join(t.Labels, " "))
	if strings.Contains(meta, "type: task") {
		return "feature"
	}
	if bugWord.MatchString(labels+" "+meta) || titleBug.MatchString(t.Title) {
		return "fix"
	}
	if strings.Contains(meta, "type: issue") && !featureWord.MatchString(labels) {
		return "fix"
	}
	return "feature"
}

var (
	badRef   = regexp.MustCompile(`[\s~^:?*\[\\]+`)
	dotsAt   = regexp.MustCompile(`\.{2,}|@\{`)
	slashes  = regexp.MustCompile(`/{2,}`)
	edges    = regexp.MustCompile(`^[/.-]+|[/.-]+$`)
	trySufix = regexp.MustCompile(`-try\d+$`)
)

// BranchName from a pattern like "{type}/{id}-{title}"; a fresh attempt gets "-try2".
func BranchName(pattern string, t core.Task, attempt int) string {
	id := t.ID
	if i := strings.LastIndex(id, "#"); i >= 0 {
		id = id[i+1:]
	}
	name := strings.NewReplacer(
		"{type}", TaskType(t),
		"{id}", id,
		"{title}", SlugifyTitle(t.Title, 40),
		"{slug}", trySufix.ReplaceAllString(t.Slug, ""),
	).Replace(pattern)
	name = badRef.ReplaceAllString(name, "-")
	name = dotsAt.ReplaceAllString(name, "-")
	name = slashes.ReplaceAllString(name, "/")
	name = edges.ReplaceAllString(name, "")
	if attempt > 1 {
		name = fmt.Sprintf("%s-try%d", name, attempt)
	}
	if name == "" {
		return "mako/" + t.Slug
	}
	return name
}

// Run runs git in dir and returns its output, or an error with what git said.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	// Cancelled, git is killed but its ssh may hold the pipes: Run does not wait for it long.
	cmd.WaitDelay = 2 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), lastLines(msg, 2))
	}
	return strings.TrimSpace(out.String()), nil
}

// One git operation at a time per repo: two tasks starting together would race for .git/config.
var repoLocks sync.Map

func lock(path string) func() {
	m, _ := repoLocks.LoadOrStore(filepath.Clean(path), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// EnsureClone clones the repo from its CloneURL when its folder has none (no prompts: an SSH key or a
// credential helper has to do) and settles its base branch when none is set.
func EnsureClone(ctx context.Context, r *config.Repo, log func(string)) error {
	defer lock(r.Path)()
	if _, err := os.Stat(filepath.Join(r.Path, ".git")); err != nil {
		if entries, _ := os.ReadDir(r.Path); len(entries) > 0 {
			return fmt.Errorf("%s не порожня і не git-репозиторій: вкажіть у налаштуваннях інший шлях", r.Path)
		}
		if r.CloneURL == "" {
			return fmt.Errorf("немає клону в %s: вкажіть у налаштуваннях шлях до репозиторію або «Звідки клонувати»", r.Path)
		}
		if err := os.MkdirAll(filepath.Dir(r.Path), 0o755); err != nil {
			return err
		}
		log(fmt.Sprintf("cloning %s into %s", r.CloneURL, r.Path))
		cctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		if _, err := Run(cctx, filepath.Dir(r.Path), "-c", "core.sshCommand=ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new", "clone", r.CloneURL, r.Path); err != nil {
			_ = os.RemoveAll(r.Path)
			return fmt.Errorf("не вдалось склонувати %s: %w", r.CloneURL, err)
		}
	}
	if r.BaseBranch == "" {
		if _, err := os.Stat(filepath.Join(r.Path, ".git", "refs", "remotes", "origin", "HEAD")); err != nil {
			_, _ = Run(ctx, r.Path, "remote", "set-head", "origin", "--auto")
		}
		r.BaseBranch = DetectBaseBranch(r.Path)
	}
	return nil
}

// DetectBaseBranch: origin/HEAD, else whichever of main/master/develop the remote has, else main. repo may be a
// worktree too, where .git is a file: git itself answers first.
func DetectBaseBranch(repo string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := Run(ctx, repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && strings.HasPrefix(out, "origin/") {
		return strings.TrimPrefix(out, "origin/")
	}
	for _, b := range []string{"main", "master", "develop"} {
		if _, err := Run(ctx, repo, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+b); err == nil {
			return b
		}
	}
	gitDir := filepath.Join(repo, ".git")
	if b, err := os.ReadFile(filepath.Join(gitDir, "refs", "remotes", "origin", "HEAD")); err == nil {
		if ref := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "ref: refs/remotes/origin/")); ref != "" && !strings.HasPrefix(ref, "ref:") {
			return ref
		}
	}
	packed, _ := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	for _, b := range []string{"main", "master", "develop"} {
		if _, err := os.Stat(filepath.Join(gitDir, "refs", "remotes", "origin", b)); err == nil || bytes.Contains(packed, []byte(" refs/remotes/origin/"+b+"\n")) {
			return b
		}
	}
	return "main"
}

// OriginURL of a clone, "" when it has none. The error is only the context's: git was cut short, so "" says
// nothing about the clone.
func OriginURL(ctx context.Context, repo string) (string, error) {
	// As configured: where the code lives, not how git reaches it (url.*.insteadOf).
	out, _ := Run(ctx, repo, "config", "--get", "remote.origin.url")
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return out, nil
}

var remotePath = regexp.MustCompile(`^(?:[a-z+]+://)?(?:[^@/]+@)?[^:/]+(?::\d+)?[:/](.+?)(?:\.git)?/?$`)

// RemoteProject: "git@host:group/app.git" or "https://host/group/app" → "group/app".
func RemoteProject(remote string) string {
	m := remotePath.FindStringSubmatch(strings.TrimSpace(remote))
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

var roots = []string{"", "projects", "Projects", "code", "Code", "src", "dev", "Dev", "Developer", "work", "Work", "repos", "git",
	"GitHub", "IdeaProjects", "WebstormProjects", "PycharmProjects", "Documents", "Documents/GitHub", "Documents/Projects",
	"Desktop", "go/src", "mako/projects"}

var skip = map[string]bool{"node_modules": true, "Library": true, "Applications": true, "Pictures": true, "Music": true, "Movies": true, "vendor": true, "target": true, "dist": true, "build": true}

// FindClone looks for a clone of a project ("group/app") in the usual places under home, a few levels deep.
func FindClone(home, project string) string {
	want := strings.ToLower(strings.Trim(project, "/"))
	budget := 6000
	seen := map[string]int{}
	var found string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if found != "" || budget <= 0 {
			return
		}
		budget--
		if d, ok := seen[dir]; ok && d >= depth {
			return
		}
		seen[dir] = depth
		if cfg, err := os.ReadFile(filepath.Join(dir, ".git", "config")); err == nil {
			for _, line := range strings.Split(string(cfg), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "url = ") {
					if p := RemoteProject(strings.TrimPrefix(line, "url = ")); p == want || strings.HasSuffix(p, "/"+want) {
						found = dir
						return
					}
				}
			}
			return // no clones inside clones
		}
		if depth == 0 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !skip[e.Name()] {
				walk(filepath.Join(dir, e.Name()), depth-1)
			}
		}
	}
	for _, r := range roots {
		depth := 3
		if r == "" {
			depth = 1
		}
		walk(filepath.Join(home, r), depth)
	}
	return found
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " ")
}
