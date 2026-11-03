package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LatestReleaseAPI: Mako's latest release on GitHub, read without a token.
const LatestReleaseAPI = "https://api.github.com/repos/SergeyPirogov/mako-agent/releases/latest"

// Release is a newer Mako the board offers to download.
type Release struct {
	Version string `json:"version"`         // "0.4.0"
	URL     string `json:"url"`             // the release's page
	Notes   string `json:"notes,omitempty"` // the start of its description
}

// Releases tells a Mako installed from a release (no clone to update from) that a newer one is out. It only
// tells: downloading is up to the user. A failed look stays quiet, like the GitHub API's rate limit.
type Releases struct {
	Current string // the running Mako's version: "0.3.0" or "v0.3.0"
	API     string // "": LatestReleaseAPI
	Client  *http.Client
	// File keeps the version the banner was hidden for, so it stays hidden after a restart; "": in memory only.
	File string

	mu        sync.Mutex
	latest    *Release
	dismissed string
	loaded    bool
	checked   int64  // ms: the last look at GitHub that got an answer
	lastErr   string // why the last look failed; "" when it went well
}

// IsRelease: the version is a release's (x.y.z), not a build from a clone ("0.3.0-12-gabc1234") or "dev".
func IsRelease(version string) bool {
	_, ok := semver(version)
	return ok
}

var semverRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

func semver(v string) ([3]int, bool) {
	var n [3]int
	m := semverRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return n, false
	}
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	return n, true
}

// newer: a is a later release than b.
func newer(a, b string) bool {
	x, ok1 := semver(a)
	y, ok2 := semver(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// Check asks GitHub for the latest release. It keeps when it last got an answer and why a look failed, for
// Checked; a failed look leaves the release found before as it was.
func (r *Releases) Check(ctx context.Context) error {
	if !IsRelease(r.Current) {
		return nil // a build from a clone updates by commits
	}
	rel, err := r.fetch(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.lastErr = err.Error()
		return err
	}
	r.latest, r.lastErr, r.checked = rel, "", time.Now().UnixMilli()
	return nil
}

// fetch reads the latest release from GitHub: nil when it is not newer than the running Mako.
func (r *Releases) fetch(ctx context.Context) (*Release, error) {
	api := r.API
	if api == "" {
		api = LatestReleaseAPI
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub: %s", res.Status)
	}
	var body struct {
		Tag   string `json:"tag_name"`
		URL   string `json:"html_url"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
		Pre   bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Draft || body.Pre || !newer(body.Tag, r.Current) {
		return nil, nil
	}
	return &Release{Version: strings.TrimPrefix(body.Tag, "v"), URL: body.URL, Notes: notes(body.Body)}, nil
}

// Watch looks for a new release now and every interval until ctx ends; failures stay quiet.
func (r *Releases) Watch(ctx context.Context, every time.Duration) {
	if !IsRelease(r.Current) {
		return
	}
	for {
		r.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Latest: the newer release to show, nil when there is none or its banner was hidden.
func (r *Releases) Latest() *Release {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.load()
	if r.latest == nil || r.latest.Version == r.dismissed {
		return nil
	}
	rel := *r.latest
	return &rel
}

// Found: the newer release the last look found, even when its banner was hidden; nil when there is none.
func (r *Releases) Found() *Release {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latest == nil {
		return nil
	}
	rel := *r.latest
	return &rel
}

// Checked: when (ms) GitHub last answered, 0 before the first answer, and why the last look failed ("" when it did not).
func (r *Releases) Checked() (int64, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checked, r.lastErr
}

// Dismiss hides the banner of the version until a later one is out.
func (r *Releases) Dismiss(version string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.load()
	r.dismissed = strings.TrimPrefix(version, "v")
	if r.File == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"dismissed": r.dismissed})
	if err := os.MkdirAll(filepath.Dir(r.File), 0o755); err != nil {
		return err
	}
	return os.WriteFile(r.File, b, 0o600)
}

// load reads the hidden version once; r.mu is held.
func (r *Releases) load() {
	if r.loaded || r.File == "" {
		return
	}
	r.loaded = true
	var saved struct {
		Dismissed string `json:"dismissed"`
	}
	if b, err := os.ReadFile(r.File); err == nil && json.Unmarshal(b, &saved) == nil {
		r.dismissed = saved.Dismissed
	}
}

// notes: the first lines of the release's description, without Markdown headings, in at most 280 characters.
func notes(body string) string {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#"))
		if l == "" {
			continue
		}
		lines = append(lines, l)
		if len(lines) == 3 {
			break
		}
	}
	s := []rune(strings.Join(lines, " "))
	if len(s) > 280 {
		return strings.TrimSpace(string(s[:279])) + "…"
	}
	return string(s)
}
