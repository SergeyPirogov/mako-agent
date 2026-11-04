package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
	"github.com/SergeyPirogov/mako-agent/go/internal/state"
	"github.com/SergeyPirogov/mako-agent/go/internal/update"
)

func TestBoard(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	store.Update("grp/app#5", func(r *core.Record) { r.Status, r.Task = core.Snoozed, core.Task{ID: "grp/app#5", Title: "Pay"} })
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), Notifications: config.Notifications{SnoozeMin: 60}}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()

	res, _ := http.Get(srv.URL + "/")
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d", res.StatusCode)
	}
	// The header has a place for the running version, outside the logo text a phone hides.
	if page, _ := io.ReadAll(res.Body); !strings.Contains(string(page), `<span class="logo-t"> Mako</span> <span class="ver" id="ver"></span>`) {
		t.Fatal("no version in the header")
	}

	// Another site in the browser cannot press the buttons.
	req, _ := http.NewRequest("POST", srv.URL+"/api/later?id=grp/app%235", nil)
	req.Header.Set("Origin", "https://evil.example")
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 403 {
		t.Fatalf("cross-site POST: %d", res.StatusCode)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/later?id=grp/app%235&min=30", nil)
	req.Header.Set("Origin", srv.URL)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 200 {
		t.Fatalf("POST: %d", res.StatusCode)
	}

	// «Запустити зараз» reaches Push: the queued task leaves the queue (here it fails at once: no trackers).
	store.Update("grp/app#6", func(r *core.Record) { r.Status, r.Task = core.Queued, core.Task{ID: "grp/app#6", Title: "Ship"} })
	req, _ = http.NewRequest("POST", srv.URL+"/api/push?id=grp/app%236", nil)
	req.Header.Set("Origin", srv.URL)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 200 {
		t.Fatalf("push: %d", res.StatusCode)
	}
	d.Wait()
	if r, _ := store.Get("grp/app#6"); r.Status == core.Queued {
		t.Fatal("pushed task still queued")
	}
	store.Delete("grp/app#6")

	// A model from the button's arrow: picked only, the task does not start, «план спочатку» stays.
	store.Update("grp/app#7", func(r *core.Record) {
		r.Status, r.Task, r.PlanFirst = core.Asking, core.Task{ID: "grp/app#7", Title: "Pick"}, true
	})
	post := func(q string) (ok bool) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/pick?id=grp/app%237"+q, nil)
		req.Header.Set("Origin", srv.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("pick %s: %v %v", q, err, res)
		}
		var reply struct{ OK *bool }
		json.NewDecoder(res.Body).Decode(&reply)
		return reply.OK == nil || *reply.OK
	}
	if !post("&model=sonnet") {
		t.Fatal("pick refused")
	}
	d.Wait()
	if r, _ := store.Get("grp/app#7"); r.Model != "sonnet" || !r.PlanFirst || r.Status != core.Asking {
		t.Fatalf("pick: model %q, plan %v, status %v", r.Model, r.PlanFirst, r.Status)
	}
	// «Як у налаштуваннях» (model=): the picked model goes, the plan stays.
	post("&model=")
	if r, _ := store.Get("grp/app#7"); r.Model != "" || !r.PlanFirst {
		t.Fatalf("pick of the default: model %q, plan %v", r.Model, r.PlanFirst)
	}
	// An explicit plan=0 turns the plan off.
	post("&model=opus&plan=0")
	if r, _ := store.Get("grp/app#7"); r.Model != "opus" || r.PlanFirst {
		t.Fatalf("pick with plan=0: model %q, plan %v", r.Model, r.PlanFirst)
	}
	// Taken already (queued from another tab): the model stays, the reply says why.
	store.Update("grp/app#7", func(r *core.Record) { r.Status = core.Queued })
	if post("&model=haiku") {
		t.Fatal("pick of a queued task was accepted")
	}
	if r, _ := store.Get("grp/app#7"); r.Model != "opus" {
		t.Fatalf("pick of a queued task changed the model to %q", r.Model)
	}
	store.Delete("grp/app#7")

	res, _ = http.Get(srv.URL + "/api/state")
	var st stateReply
	json.NewDecoder(res.Body).Decode(&st)
	if len(st.Records) != 1 || !st.Records[0].Postponed || st.Records[0].Column != "new" {
		t.Fatalf("state %+v", st.Records)
	}
	// The state carries the version the header shows.
	if st.Version != "test" {
		t.Fatalf("version %q", st.Version)
	}

	// «Перезапустити, коли агенти закінчать хід»: asked for, then taken back.
	req, _ = http.NewRequest("POST", srv.URL+"/api/restart?on=1", nil)
	req.Header.Set("Origin", srv.URL)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 200 || !d.RestartPending() {
		t.Fatalf("restart: %d, pending %v", res.StatusCode, d.RestartPending())
	}
	res, _ = http.Get(srv.URL + "/api/state")
	st = stateReply{}
	json.NewDecoder(res.Body).Decode(&st)
	if !st.Restart.Pending || len(st.Restart.Busy) != 0 {
		t.Fatalf("restart in state: %+v", st.Restart)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/restart?on=0", nil)
	req.Header.Set("Origin", srv.URL)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 200 || d.RestartPending() {
		t.Fatalf("restart taken back: %d, pending %v", res.StatusCode, d.RestartPending())
	}
}

// Before the take the board names the model the agent gets when none is picked: the estimate's, else Sonnet.
func TestBoardDefaultModel(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	plain := core.Task{ID: "grp/app#1", Title: "a"}
	advised := core.Task{ID: "grp/app#2", Title: "b"}
	store.Update(plain.ID, func(r *core.Record) { r.Status, r.Task = core.Snoozed, plain })
	store.Update(advised.ID, func(r *core.Record) {
		r.Status, r.Task, r.Model = core.Snoozed, advised, "haiku" // picked on the card: the default stays
		r.Estimate = &core.Estimate{State: "done", Size: "big", Model: "opus", For: dispatch.EstimateKey(advised)}
	})
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), Executor: "terminal", EstimateModel: true}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/state")
	var st stateReply
	json.NewDecoder(res.Body).Decode(&st)
	m := map[string]string{}
	for _, r := range st.Records {
		m[r.ID] = r.DefaultModel
	}
	if m[plain.ID] != "sonnet" || m[advised.ID] != "opus" {
		t.Fatalf("default models %v", m)
	}
}

// A queued task tells the board why it waits; the board has a «запускаю» state for a task being started.
func TestBoardQueueWhy(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	store.Update("grp/app#1", func(r *core.Record) { r.Status, r.Task = core.HandedOff, core.Task{ID: "grp/app#1", Title: "a"} })
	store.Update("grp/app#2", func(r *core.Record) { r.Status, r.Task = core.Queued, core.Task{ID: "grp/app#2", Title: "b"} })
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), WipLimit: 1}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	res, _ := http.Get(srv.URL + "/api/state")
	var st stateReply
	json.NewDecoder(res.Body).Decode(&st)
	for _, r := range st.Records {
		want := map[string]string{"grp/app#2": "WIP limit: 1 of 1 in work"}[r.ID]
		if r.QueueWhy != want || r.Starting != nil {
			t.Fatalf("%s: queueWhy %q, starting %+v", r.ID, r.QueueWhy, r.Starting)
		}
	}
	res, _ = http.Get(srv.URL + "/")
	if page, _ := io.ReadAll(res.Body); !strings.Contains(string(page), `"queued", "starting"`) || i18n.T("b.label.starting") != "starting…" {
		t.Fatal("no \"starting\" state on the board")
	}
}

func TestSettingsSaveReloads(t *testing.T) {
	home := t.TempDir()
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: home}).Handler())
	defer srv.Close()
	post := func(body string) map[string]any {
		req, _ := http.NewRequest("POST", srv.URL+"/api/settings", strings.NewReader(body))
		req.Header.Set("Origin", srv.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	// Something Mako could not read is refused and nothing is written.
	if r := post(`{"config":{"trackers":[{"type":"svn"}]}}`); r["ok"] != false {
		t.Fatalf("bad config saved: %v", r)
	}
	// A token on two lines would break .env: refused before mako.config.json is written.
	if r := post(`{"config":{"wipLimit":3},"env":{"GITHUB_TOKEN":"a\nB=1"}}`); r["ok"] != false || !strings.Contains(r["error"].(string), "GITHUB_TOKEN") {
		t.Fatalf("a token on two lines saved: %v", r)
	}
	if _, err := os.Stat(filepath.Join(home, "mako.config.json")); err == nil {
		t.Fatal("mako.config.json written for a refused save")
	}
	r := post(`{"config":{"wipLimit":2,"keepMe":{"x":1},"trackers":[{"type":"github","allProjects":true}]},"env":{"GITHUB_TOKEN":"tok"}}`)
	if r["ok"] != true {
		t.Fatalf("save: %v", r)
	}
	if d.Config().WipLimit != 2 || len(d.TrackerList()) != 1 {
		t.Fatalf("not reloaded: %+v %v", d.Config(), d.TrackerList())
	}
	data, _ := os.ReadFile(filepath.Join(home, "mako.config.json"))
	if !strings.Contains(string(data), `"keepMe"`) {
		t.Fatalf("unknown settings lost:\n%s", data)
	}
	res, _ := http.Get(srv.URL + "/api/settings")
	var got settingsReply
	json.NewDecoder(res.Body).Decode(&got)
	if !got.Env["GITHUB_TOKEN"] || got.Env["GITLAB_TOKEN"] {
		t.Fatalf("env %v", got.Env)
	}
	if strings.Contains(string(got.Config), "tok") {
		t.Fatal("the token leaked into the page")
	}
}

// The model of «Запитати про код» picked on the board goes to the settings, the rest of them as they were.
func TestCodeModel(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "mako.config.json"), []byte(`{"wipLimit":3,"codeAsk":{"other":1}}`), 0o600)
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), CodeModel: config.DefaultCodeModel}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: home}).Handler())
	defer srv.Close()
	models := func() (out struct {
		Models  []string
		Current string
	}) {
		res, _ := http.Get(srv.URL + "/api/code/models")
		json.NewDecoder(res.Body).Decode(&out)
		return
	}
	if m := models(); strings.Join(m.Models, ",") != "opus,sonnet,haiku" || m.Current != "haiku" {
		t.Fatalf("models %+v", m)
	}
	// Only a model's name: it goes to claude's --model.
	bad, _ := http.NewRequest("POST", srv.URL+"/api/code/model", strings.NewReader(`{"model":"--dangerously-skip-permissions"}`))
	bad.Header.Set("Origin", srv.URL)
	if res, err := http.DefaultClient.Do(bad); err != nil {
		t.Fatal(err)
	} else {
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		if out["ok"] != false || d.Config().CodeModel != config.DefaultCodeModel {
			t.Fatalf("bad model saved: %v", out)
		}
	}
	// A well-formed name that the picker does not offer is refused too.
	unk, _ := http.NewRequest("POST", srv.URL+"/api/code/model", strings.NewReader(`{"model":"zzz"}`))
	unk.Header.Set("Origin", srv.URL)
	if res, err := http.DefaultClient.Do(unk); err != nil {
		t.Fatal(err)
	} else {
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		if out["ok"] != false || out["error"] != "unknown model" || d.Config().CodeModel != config.DefaultCodeModel {
			t.Fatalf("unknown model saved: %v", out)
		}
		if data, _ := os.ReadFile(filepath.Join(home, "mako.config.json")); strings.Contains(string(data), "zzz") {
			t.Fatalf("config %s", data)
		}
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/code/model", strings.NewReader(`{"model":"opus"}`))
	req.Header.Set("Origin", srv.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	if out["ok"] != true || d.Config().CodeModel != "opus" || d.Config().WipLimit != 3 {
		t.Fatalf("saved %v, model %q", out, d.Config().CodeModel)
	}
	if data, _ := os.ReadFile(filepath.Join(home, "mako.config.json")); !strings.Contains(string(data), `"other": 1`) || !strings.Contains(string(data), `"model": "opus"`) {
		t.Fatalf("config %s", data)
	}
	if m := models(); m.Current != "opus" {
		t.Fatalf("models %+v", m)
	}
}

func TestPhone(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	phones, _ := phone.Open(t.TempDir())
	srv := httptest.NewServer((&Server{D: d, Phones: phones}).Handler())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Through `tailscale serve`: from 127.0.0.1, with forwarding headers.
	get := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("X-Forwarded-For", "100.101.102.103")
		req.Header.Set("X-Forwarded-Proto", "https")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := get("/api/state"); res.StatusCode != 401 {
		t.Fatalf("unpaired phone got the state: %d", res.StatusCode)
	}
	if res := get("/"); res.StatusCode != 401 {
		t.Fatalf("unpaired phone got the board: %d", res.StatusCode)
	}
	// The service worker's icons, Android's status-bar badge among them, load without a key.
	for _, p := range []string{"/icons/icon-192.png", "/icons/badge-96.png"} {
		if res := get(p); res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("%s: %d", p, res.StatusCode)
		}
	}
	post := func(code string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/pair", strings.NewReader("code="+code))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", "100.101.102.103")
		req.Header.Set("Origin", srv.URL)
		req.Header.Set("X-Forwarded-Proto", "https")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := get("/pair"); res.StatusCode != 200 {
		t.Fatalf("pair page: %d", res.StatusCode)
	}
	// A code in the query is not read any more: it must travel in the fragment.
	if res := get("/pair?code=" + phones.NewPairing()); res.StatusCode != 200 || len(res.Cookies()) != 0 {
		t.Fatalf("a query code paired: %d", res.StatusCode)
	}
	if res := post("guess"); res.StatusCode != 401 {
		t.Fatalf("a made-up code paired: %d", res.StatusCode)
	}
	// The QR's code pairs the phone once.
	res := post(phones.NewPairing())
	if res.StatusCode != 200 {
		t.Fatalf("pair: %d", res.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == deviceCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure {
		t.Fatalf("cookie %+v", cookie)
	}
	if res := get("/api/state"); res.StatusCode != 200 {
		t.Fatalf("paired phone: %d", res.StatusCode)
	}
	var st stateReply
	json.NewDecoder(get("/api/state").Body).Decode(&st)
	if !st.Remote {
		t.Fatal("a phone's state should say so")
	}
	// Settings stay on the Mac.
	for _, p := range []string{"/settings", "/api/settings", "/api/phone"} {
		if res := get(p); res.StatusCode != 403 {
			t.Fatalf("%s from the phone: %d", p, res.StatusCode)
		}
	}
	// The installed app signs itself in from its start_url.
	var manifest struct {
		StartURL string `json:"start_url"`
	}
	json.NewDecoder(get("/manifest.webmanifest").Body).Decode(&manifest)
	if !strings.HasPrefix(manifest.StartURL, "/pair/d/") {
		t.Fatalf("start_url %q", manifest.StartURL)
	}
	jar2, _ := cookiejar.New(nil)
	client.Jar = jar2
	if res := get(manifest.StartURL); res.StatusCode != 302 {
		t.Fatalf("start_url: %d", res.StatusCode)
	}
	if res := get("/api/state"); res.StatusCode != 200 {
		t.Fatalf("app after start_url: %d", res.StatusCode)
	}
	// Pushes: the app subscribes.
	req, _ := http.NewRequest("POST", srv.URL+"/api/push/subscribe", strings.NewReader(`{"endpoint":"https://web.push.apple.com/x","keys":{"auth":"a","p256dh":"b"}}`))
	req.Header.Set("X-Forwarded-For", "100.101.102.103")
	req.Header.Set("Origin", srv.URL)
	if res, _ := client.Do(req); res.StatusCode != 200 {
		t.Fatalf("subscribe: %d", res.StatusCode)
	}
	if list := phones.Devices(); len(list) != 1 || list[0].Push == nil {
		t.Fatalf("devices %+v", list)
	}
	// This Mac itself needs no key.
	if res, _ := http.Get(srv.URL + "/settings"); res.StatusCode != 200 {
		t.Fatalf("Mac settings: %d", res.StatusCode)
	}
}

func TestUpdate(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	started := make(chan bool, 1)
	u := &update.Updater{Source: t.TempDir(), Exe: filepath.Join(t.TempDir(), "mako"),
		Build: func(context.Context, string, string) error { started <- true; return errors.New("no") }}
	srv := httptest.NewServer((&Server{D: d, Update: u}).Handler())
	defer srv.Close()

	res, _ := http.Get(srv.URL + "/api/state")
	var st stateReply
	json.NewDecoder(res.Body).Decode(&st)
	if st.Update == nil {
		t.Fatal("no update status on the Mac")
	}
	// A phone sees no update and cannot start one.
	req, _ := http.NewRequest("POST", srv.URL+"/api/update", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.2")
	if res, _ := http.DefaultClient.Do(req); res.StatusCode == 200 || u.Status().State != "" {
		t.Fatalf("from a phone: %d", res.StatusCode)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/update", nil)
	req.Header.Set("Origin", srv.URL)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 200 {
		t.Fatalf("update: %d", res.StatusCode)
	}
	// Not a repository: the update fails before building, and says so.
	for i := 0; i < 100 && u.Status().State != "failed"; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if s := u.Status(); s.State != "failed" || s.Error == "" || len(started) != 0 {
		t.Fatalf("status %+v", s)
	}
}

func TestDigest(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	store.Update("grp/app#5", func(r *core.Record) {
		r.Status, r.Task, r.PRURL = core.Done, core.Task{ID: "grp/app#5", Title: "Pay"}, "https://mr/5"
		r.FinishedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), Notifications: config.Notifications{AwayHours: 4}}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()

	// The board seen for the first time: nothing to compare with, no digest.
	var st stateReply
	res, _ := getMako(srv.URL + "/api/state?seen=1")
	json.NewDecoder(res.Body).Decode(&st)
	if st.Digest != nil {
		t.Fatalf("digest on the first look: %+v", st.Digest)
	}
	since := time.Now().Add(-2 * time.Hour).UnixMilli()
	res, _ = http.Get(srv.URL + "/api/digest?since=" + strconv.FormatInt(since, 10))
	var dg core.Digest
	json.NewDecoder(res.Body).Decode(&dg)
	if dg.Since != since || len(dg.Review) != 1 || !dg.Review[0].New || dg.Review[0].MR != "https://mr/5" {
		t.Fatalf("digest %+v", dg)
	}
}

// postMako: a POST as Mako's command line makes it, with X-Mako and no Origin.
func postMako(u, contentType string, body io.Reader) (*http.Response, error) {
	req, _ := http.NewRequest("POST", u, body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Mako", "1")
	return http.DefaultClient.Do(req)
}

// getMako: a GET as the board makes it, with X-Mako.
func getMako(u string) (*http.Response, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("X-Mako", "1")
	return http.DefaultClient.Do(req)
}

// ?seen=1 counts as «you are back» only from the board: another site's <img> does not end the absence.
func TestSeenOnlyFromBoard(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	store.Update("grp/app#5", func(r *core.Record) {
		r.Status, r.Task, r.PRURL = core.Done, core.Task{ID: "grp/app#5", Title: "Pay"}, "https://mr/5"
		r.FinishedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	dir := t.TempDir()
	away := time.Now().Add(-5 * time.Hour).UnixMilli()
	os.WriteFile(filepath.Join(dir, "digest.json"), []byte(fmt.Sprintf(`{"seen":%d}`, away)), 0o600)
	d := dispatch.New(&config.Config{StateDir: dir, Notifications: config.Notifications{AwayHours: 4}}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	look := func(res *http.Response, err error) (st stateReply) {
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(res.Body).Decode(&st)
		return
	}
	if st := look(http.Get(srv.URL + "/api/state?seen=1")); st.Digest != nil {
		t.Fatalf("digest without X-Mako: %+v", st.Digest)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "digest.json")); !strings.Contains(string(b), strconv.FormatInt(away, 10)) {
		t.Fatalf("seen moved without X-Mako: %s", b)
	}
	if st := look(getMako(srv.URL + "/api/state?seen=1")); st.Digest == nil {
		t.Fatal("no digest from the board after 5 hours away")
	}
}

// ?project= narrows the stats to one project's tasks; without it they count all of them.
func TestStateStatsByProject(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	at := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	for id, project := range map[string]string{"grp/one#1": "grp/one", "grp/one#2": "grp/one", "grp/two#1": "grp/two"} {
		store.Update(id, func(r *core.Record) {
			r.Status, r.Task, r.StartedAt, r.FinishedAt = core.Done, core.Task{ID: id, Title: id, Project: project}, at, at
		})
	}
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	stats := func(q string) core.Stats {
		res, _ := http.Get(srv.URL + "/api/state" + q)
		var st stateReply
		json.NewDecoder(res.Body).Decode(&st)
		return st.Stats
	}
	if s := stats(""); s.Taken != 3 || len(s.Projects) != 2 {
		t.Fatalf("all: %+v", s)
	}
	if s := stats("?project=grp%2Fone"); s.Taken != 2 || len(s.Projects) != 1 || s.Projects[0].Name != "grp/one" {
		t.Fatalf("grp/one: %+v", s)
	}
	if s := stats("?project=grp%2Fnone&days=7"); s.Taken != 0 {
		t.Fatalf("none: %+v", s)
	}
}

// ?days=today counts the tasks taken since the local midnight only: not yesterday's.
func TestStateStatsToday(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	n := time.Now()
	midnight := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location())
	stamp := func(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }
	for id, at := range map[string]time.Time{
		"grp/one#1": midnight.Add(-time.Hour),          // yesterday
		"grp/one#2": midnight.Add(n.Sub(midnight) / 2), // today
	} {
		store.Update(id, func(r *core.Record) {
			r.Status, r.Task, r.StartedAt, r.FinishedAt = core.Done, core.Task{ID: id, Title: id, Project: "grp/one"}, stamp(at), stamp(at)
		})
	}
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	stats := func(q string) core.Stats {
		res, _ := http.Get(srv.URL + "/api/state" + q)
		var st stateReply
		json.NewDecoder(res.Body).Decode(&st)
		return st.Stats
	}
	if s := stats("?days=today"); s.Taken != 1 {
		t.Fatalf("today: %+v", s)
	}
	if s := stats(""); s.Taken != 2 {
		t.Fatalf("all: %+v", s)
	}
}

// «Пауза» of one tracker from the board writes "paused" to its entry only and reloads Mako.
func TestTrackerPause(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".env"), []byte("GITHUB_TOKEN=a\nGITLAB_TOKEN=b\n"), 0o600)
	os.WriteFile(filepath.Join(home, "mako.config.json"), []byte(`{"wipLimit":2,"trackers":[
	  {"type":"github","allProjects":true},{"type":"gitlab","id":"work","baseUrl":"https://git.example.com","allProjects":true}]}`), 0o644)
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Home: home}).Handler())
	defer srv.Close()
	pause := func(id, on string) map[string]any {
		req, _ := http.NewRequest("POST", srv.URL+"/api/tracker-pause?id="+id+"&on="+on, nil)
		req.Header.Set("Origin", srv.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	paused := func() map[string]bool {
		out := map[string]bool{}
		for _, tr := range d.TrackerList() {
			out[tr.ID()] = tr.Config().Paused
		}
		return out
	}
	if r := pause("work", "1"); r["ok"] != true {
		t.Fatalf("pause: %v", r)
	}
	if p := paused(); !p["work"] || p["github"] || len(p) != 2 {
		t.Fatalf("after pause: %v", p)
	}
	data, _ := os.ReadFile(filepath.Join(home, "mako.config.json"))
	if strings.Count(string(data), `"paused"`) != 1 || !strings.Contains(string(data), `"wipLimit": 2`) {
		t.Fatalf("config:\n%s", data)
	}
	if r := pause("work", "0"); r["ok"] != true || paused()["work"] {
		t.Fatalf("resume: %v %v", r, paused())
	}
	if data, _ := os.ReadFile(filepath.Join(home, "mako.config.json")); strings.Contains(string(data), `"paused"`) {
		t.Fatalf("paused left after resume:\n%s", data)
	}
	if r := pause("nope", "1"); r["ok"] != false {
		t.Fatalf("unknown tracker: %v", r)
	}
}

func TestRelease(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"v0.4.0","html_url":"https://example.com/r","body":"New"}`))
	}))
	defer api.Close()
	rel := &update.Releases{Current: "0.3.0", API: api.URL, File: filepath.Join(t.TempDir(), "release.json")}
	rel.Check(context.Background())
	srv := httptest.NewServer((&Server{D: d, Release: rel}).Handler())
	defer srv.Close()

	get := func(phone bool) *update.Release {
		req, _ := http.NewRequest("GET", srv.URL+"/api/state", nil)
		if phone {
			req.Header.Set("X-Forwarded-For", "100.64.0.2")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var st stateReply
		json.NewDecoder(res.Body).Decode(&st)
		return st.Release
	}
	if r := get(false); r == nil || r.Version != "0.4.0" || r.URL != "https://example.com/r" {
		t.Fatalf("release %+v", r)
	}
	if get(true) != nil {
		t.Fatal("a phone sees the release")
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/release/dismiss?version=0.4.0", nil)
	req.Header.Set("Origin", srv.URL)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 200 {
		t.Fatalf("dismiss: %d", res.StatusCode)
	}
	if get(false) != nil {
		t.Fatal("shown after hiding")
	}
}

// makoCheck: POST /api/update/check (the Mac's own request unless phone) and the reply.
func makoCheck(t *testing.T, srv *httptest.Server, phone bool, origin string) (int, makoInfo) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/api/update/check", nil)
	req.Header.Set("Origin", origin)
	if phone {
		req.Header.Set("X-Forwarded-For", "100.64.0.2")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var info makoInfo
	json.NewDecoder(res.Body).Decode(&info)
	return res.StatusCode, info
}

func TestUpdateCheckRelease(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	fail := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "limit", http.StatusForbidden)
			return
		}
		w.Write([]byte(`{"tag_name":"v0.4.0","html_url":"https://example.com/r","body":"New"}`))
	}))
	defer api.Close()
	rel := &update.Releases{Current: "0.3.0", API: api.URL, File: filepath.Join(t.TempDir(), "release.json")}
	srv := httptest.NewServer((&Server{D: d, Version: "0.3.0", Release: rel}).Handler())
	defer srv.Close()

	code, info := makoCheck(t, srv, false, srv.URL)
	if code != 200 || !info.OK || info.Kind != "release" || info.Version != "0.3.0" || info.Checked == 0 ||
		info.Release == nil || info.Release.Version != "0.4.0" || info.Release.URL != "https://example.com/r" {
		t.Fatalf("check: %d %+v", code, info)
	}
	// The release stays in the settings after its banner was hidden.
	req, _ := http.NewRequest("POST", srv.URL+"/api/release/dismiss?version=0.4.0", nil)
	req.Header.Set("Origin", srv.URL)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 200 {
		t.Fatalf("dismiss: %d", res.StatusCode)
	}
	if _, info = makoCheck(t, srv, false, srv.URL); info.Release == nil || info.Release.Version != "0.4.0" {
		t.Fatalf("after hiding: %+v", info)
	}
	res, _ := http.Get(srv.URL + "/api/update/info")
	info = makoInfo{}
	json.NewDecoder(res.Body).Decode(&info)
	if info.Release == nil || info.Kind != "release" {
		t.Fatalf("info: %+v", info)
	}
	// The look fails: the reason is shown, the release found before stays.
	fail = true
	if _, info = makoCheck(t, srv, false, srv.URL); info.OK || info.Error == "" || info.Release == nil {
		t.Fatalf("failed look: %+v", info)
	}
	res, _ = http.Get(srv.URL + "/api/update/info")
	info = makoInfo{}
	json.NewDecoder(res.Body).Decode(&info)
	if info.OK || info.Error == "" {
		t.Fatalf("info after a failed look: %+v", info)
	}
}

func TestUpdateCheckClone(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	// Not a repository: the look fails, and says so.
	srv := httptest.NewServer((&Server{D: d, Version: "0.3.0-2-gabc", Update: &update.Updater{Source: t.TempDir()}}).Handler())
	defer srv.Close()
	code, info := makoCheck(t, srv, false, srv.URL)
	if code != 200 || info.OK || info.Kind != "clone" || info.Error == "" || info.Update == nil {
		t.Fatalf("check: %d %+v", code, info)
	}
}

func TestUpdateCheckDevBuild(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "dev"}).Handler())
	defer srv.Close()
	code, info := makoCheck(t, srv, false, srv.URL)
	if code != 200 || info.OK || info.Kind != "dev" || info.Version != "dev" || info.Error == "" {
		t.Fatalf("check: %d %+v", code, info)
	}
}

func TestUpdateCheckIsFromTheMacOnly(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	asked := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		w.Write([]byte(`{"tag_name":"v0.4.0","html_url":"https://example.com/r"}`))
	}))
	defer api.Close()
	rel := &update.Releases{Current: "0.3.0", API: api.URL}
	srv := httptest.NewServer((&Server{D: d, Release: rel}).Handler())
	defer srv.Close()
	// A phone is stopped (401 before it is paired, 403 by the handler itself), and GitHub is not asked.
	if code, _ := makoCheck(t, srv, true, srv.URL); code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("from a phone: %d", code)
	}
	if code, _ := makoCheck(t, srv, false, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("from another site: %d", code)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/update/info", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.2")
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("info from a phone: %d", res.StatusCode)
	}
	if asked != 0 {
		t.Fatalf("GitHub was asked %d times", asked)
	}
}

func TestUpdateCancelAndNowAreFromTheMacOnly(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Update: &update.Updater{Source: t.TempDir()}}).Handler())
	defer srv.Close()
	for _, path := range []string{"/api/update/cancel", "/api/update/now"} {
		req, _ := http.NewRequest("POST", srv.URL+path, nil)
		req.Header.Set("X-Forwarded-For", "100.64.0.2")
		if res, _ := http.DefaultClient.Do(req); res.StatusCode == 200 {
			t.Fatalf("%s from a phone", path)
		}
		req, _ = http.NewRequest("POST", srv.URL+path, nil)
		req.Header.Set("Origin", srv.URL)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: %v %v", path, res, err)
		}
	}
}

// «Надіслати в трекер»: when the comment cannot be written, the reply says why, for the card to show.
func TestAnswersErrorReachesCard(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	task := core.Task{ID: "grp/app#3", Title: "c", TrackerID: "gone"}
	store.Update(task.ID, func(r *core.Record) {
		r.Status, r.Task = core.Snoozed, task
		r.Estimate = &core.Estimate{State: "done", Size: "small", Questions: "- Яка кнопка?", For: dispatch.EstimateKey(task)}
	})
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), Executor: "terminal"}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	res, err := postMako(srv.URL+"/api/answers?id="+url.QueryEscape(task.ID), "application/json", strings.NewReader(`{"answers":[{"q":"Яка кнопка?","a":"«Оплатити»"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	json.NewDecoder(res.Body).Decode(&reply)
	if reply.OK || !strings.Contains(reply.Error, "tracker gone") {
		t.Fatalf("reply %+v", reply)
	}
}

// A body that is not the answers is said to be so, not taken for no answers at all.
func TestAnswersBadBody(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir(), Executor: "terminal"}, store, nil)
	store.Update("grp/app#4", func(r *core.Record) { r.Status, r.Task = core.Snoozed, core.Task{ID: "grp/app#4"} })
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()
	res, err := postMako(srv.URL+"/api/answers?id="+url.QueryEscape("grp/app#4"), "application/json", strings.NewReader(`{"answers":["старий формат"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	json.NewDecoder(res.Body).Decode(&reply)
	if reply.OK || !strings.Contains(reply.Error, "could not read the answers") {
		t.Fatalf("reply %+v", reply)
	}
}

// DNS rebinding: a request that reaches 127.0.0.1 under another site's name is refused, with Origin matching it.
func TestForeignHostRefused(t *testing.T) {
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{StateDir: t.TempDir()}, store, nil)
	h := (&Server{D: d}).Handler()
	for host, want := range map[string]int{"evil.example:8080": http.StatusForbidden, "127.0.0.1:8080": http.StatusOK, "localhost:8080": http.StatusOK, "[::1]:8080": http.StatusOK} {
		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host, req.RemoteAddr = host, "127.0.0.1:5555"
		req.Header.Set("Origin", "http://"+host)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %s: got %d, want %d", host, rec.Code, want)
		}
	}
}

// The board's script is one classic script: a function declared twice there is not an error but the last one
// wins, silently, as the stats' stagesHTML once hid the MR card's CI jobs.
func TestBoardFunctionsOnce(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^(?:async )?function (\w+)\(`).FindAllStringSubmatch(string(board), -1) {
		if seen[m[1]] {
			t.Errorf("function %s is declared twice in board.html", m[1])
		}
		seen[m[1]] = true
	}
}

// The pages are English only: no Cyrillic in the board, the settings or the phone pages, except the Ukrainian
// examples of what the agent writes (the agentLanguage choice and its comment templates).
func TestPagesHaveNoCyrillic(t *testing.T) {
	cyr := regexp.MustCompile(`[А-Яа-яІіЇїЄєҐґ]`)
	example := regexp.MustCompile(`uk: "[^"]*"|\["uk", "[^"]*"\]`)
	for _, f := range []string{"board.html", "settings.html", "theme.js", "sw.js", "phone.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			line = example.ReplaceAllString(line, "")
			if cyr.MatchString(line) {
				t.Errorf("%s:%d has Cyrillic: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// The wording of the board's "waits for Fix" (a normal state, not a breakage) and the Fix in the merge's blockers.
// The board has no script tests, so these check the page's source: the old wording is gone, the new one is in the
// three places it is shown, the blockers' block holds its own Fix, and the phone's counter has a short label.
func TestBoardFixWording(t *testing.T) {
	src := string(board)
	if strings.Contains(src, "never reached the agent") {
		t.Error("board.html still says the remarks never reached the agent: it reads as a breakage")
	}
	const want = "The reviewer left remarks: press Fix to hand them to the agent"
	// "Needs attention" and the blockers' line (for no counted blockers) both say it.
	if n := strings.Count(src, want) + strings.Count(src, "The reviewer left ${n ?"); n < 2 {
		t.Errorf("the text %q is shown %d times, want it in \"Needs attention\" and in the blockers", want, n)
	}
	if !strings.Contains(src, "the reviewer left remarks: press Fix to hand them to the agent. The numbers above do not count them yet.") {
		t.Error("Quality's \"AI review\" card does not use the new wording")
	}
	// The Fix is in the "What blocks the merge" block itself, only while "Fix" can be pressed (step "notes").
	i := strings.Index(src, "function blockersHTML(")
	j := strings.Index(src[i:], "\n}\n")
	if i < 0 || j < 0 {
		t.Fatal("no blockersHTML in board.html")
	}
	body := src[i : i+j]
	if !strings.Contains(body, `r.reviewStep === "notes"`) || !strings.Contains(body, "reviewFixBtn(true, n)") {
		t.Error("blockersHTML has no Fix button for the reviewer's blockers")
	}
	// No "Skip review" there: SkipReview refuses while the remarks are pending, so the button would only fail.
	if strings.Contains(body, "skipBtn") || strings.Contains(body, "reviewskip") {
		t.Error("blockersHTML offers Skip review, which the server refuses while remarks are pending")
	}
	// The phone's counter keeps a short word: the long ones are hidden there.
	css := string(style)
	if !strings.Contains(src, `<span class="ws">`) || !strings.Contains(css, ".wip .sl, .wip .ws { display: inline; }") {
		t.Error("the phone's counter in the header has no short label")
	}
	for _, k := range []string{"b.wipShortOrca", "b.wipShortWork"} {
		if i18n.WebStrings()[k] == "" {
			t.Errorf("no text for %s", k)
		}
	}
}

// The board's small layout and empty-state rules (#515). The page has no browser tests, so these hold the
// source of each rule: a change that drops one fails here, and the manual check is in the MR.
func TestBoardEmptyStates(t *testing.T) {
	css, page := string(style), string(board)
	for name, c := range map[string]struct{ in, want string }{
		"a short status keeps its width":    {css, ".item:not(.rq) .m > .say { flex: none; }"},
		"the labels beside it shrink":       {css, ".item .m > :is(.checks, .remarks, .reviewed, .updated) { flex: 0 1 auto; min-width: 0;"},
		"no second Resume in the chat":      {page, `card?.primary === "resume"`},
		"the hints wait for a conversation": {page, `s.state === "waiting" || !s.items.length`},
		"no empty project select in Ask":    {page, `chatMode() === "ask" && !fcProjects.length);`},
	} {
		if !strings.Contains(c.in, c.want) {
			t.Errorf("%s: %q is missing", name, c.want)
		}
	}
	if strings.Contains(css, ".item .m > .say { flex:") {
		t.Error("a status rule for every row: a review row's long text needs its own")
	}
}

// A picture on the board opens in a lightbox over it, not in a new window (the desktop app hands those to the browser).
func TestBoardPictureLightbox(t *testing.T) {
	for _, want := range []string{
		`<dialog id="pic-dlg"`,
		`e.target.closest?.("a > img")`,
		`e.metaKey || e.ctrlKey`,
		`$("pic-dlg").showModal()`,
		`$("pic-dlg").addEventListener("click"`,
	} {
		if !strings.Contains(string(board), want) {
			t.Errorf("board.html lacks %q", want)
		}
	}
	css, err := os.ReadFile("style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#pic-dlg::backdrop", "#pic-dlg img", ".me-pics img { cursor: zoom-in"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("style.css lacks %q", want)
		}

	}
}

// The quiet ghost button first in the review block's row («Review again», «Run anyway», «Another round») is
// shifted left by its padding and border, so its text lines up with the title (#566); «Fix with the agent» keeps
// its visible border and stays put. The board has no browser tests, so this holds the stylesheet's source.
func TestReviewGhostButtonAligned(t *testing.T) {
	css, err := os.ReadFile("style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		".mr-adv-a > button.ghost:not(.fix):first-child { margin-left: -13px; }",
		".mr-adv-a button.ghost.fix { border-color: var(--line)",
	} {
		if !strings.Contains(string(css), want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
}

// The Branch card's rows hold the 26px ✨ button (#630): the row is as tall as the button, rows have a gap so the
// buttons do not touch, and the text and icon sit at the button's centre. The board has no browser tests, so this
// holds the stylesheet's source.
func TestBranchCardRowsFitFixButton(t *testing.T) {
	css, err := os.ReadFile("style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		".br-card .mr-row { min-height: 26px; }",
		".br-card .mr-row + .mr-row { margin-top: 4px; }",
		".br-card .mr-row > a, .br-card .mr-row > span { padding: 3px 0; font-size: 12.5px; }",
		".br-card .mr-row > svg, .br-card .mr-row > .mi { margin-top: 6px; }",
		".br-card .mr-row > button { margin-top: 0; }",
	} {
		if !strings.Contains(string(css), want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
}

// The fix buttons of the merge's blockers (#561): «Resolve conflicts», «Fix CI» and «Fix comments» stand inside the
// «What blocks the merge» block under their rows, and only for a live terminal agent; the header keeps Open MR.
// The board has no script tests, so these hold the page's source.
func TestBoardBlockerFixButtons(t *testing.T) {
	src := string(board)
	i := strings.Index(src, "function blockersHTML(")
	j := strings.Index(src[i:], "\n}\n")
	if i < 0 || j < 0 {
		t.Fatal("no blockersHTML in board.html")
	}
	body := src[i : i+j]
	for _, want := range []string{
		`const live = r.executor === "terminal" && r.worktree && r.live;`,
		`askBtn("conflicts", "Resolve conflicts"`,
		`askBtn("ci", "Fix CI"`,
		`askBtn("comments", "Fix comments"`,
		`Passed to the agent`,
		`isAsked(r.id, kind)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("blockersHTML is missing %q", want)
		}
	}
	// The header: only Open MR for the fix states, no button with data-ask.
	k := strings.Index(src, "function primaryHTML(")
	l := strings.Index(src[k:], "\n}\n")
	if k < 0 || l < 0 {
		t.Fatal("no primaryHTML in board.html")
	}
	head := src[k : k+l]
	if strings.Contains(head, "data-ask") {
		t.Error("primaryHTML still has a fix button: it is in the blockers' block now")
	}
	if !strings.Contains(head, `case "fix-conflicts": case "fix-ci": return openMR(r);`) {
		t.Error("the header does not keep Open MR for the fix states")
	}
}

// Open PR / Open MR in the task card's header is a secondary square outline button with only the host's logo
// (#586, #631), the same in every state; the name is in the title and aria-label. The page has no script
// tests, so this holds the source.
func TestBoardOpenMRButtonSecondary(t *testing.T) {
	src := string(board)
	i := strings.Index(src, "const openMR = ")
	j := strings.Index(src[i:], "\n};\n")
	if i < 0 || j < 0 {
		t.Fatal("no openMR in board.html")
	}
	body := src[i : i+j]
	for _, want := range []string{
		`r.prUrl.includes("/pull/") ? "GitHub" : r.prUrl.includes("merge_requests") ? "GitLab" : ""`,
		`TRACKER_ICON[host]`,
		`: ICON.external`,
		`class="btn open-mr icon-only"`,
		`title="${label}"`,
		`aria-label="${label}"`,
		`on ${host}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("openMR is missing %q", want)
		}
	}
	if strings.Contains(body, "primary") {
		t.Error("openMR is a primary button again")
	}
	if strings.Contains(body, ">Open ${") || strings.Contains(body, "${ICON.external}</a>") {
		t.Error("openMR has visible text or the arrow again: it is only the logo")
	}
	if strings.Contains(src, `openMR(r, "primary")`) {
		t.Error("a state of the header still asks for a primary Open MR")
	}
	css := string(style)
	for _, want := range []string{
		".d-head .actions > a.btn.open-mr { width: 32px; padding: 0; justify-content: center; }",
		".d-head .actions > a.btn svg { flex: none; vertical-align: -3px; }",
		".d-head .actions > a.btn.open-mr > svg { width: 16px; height: 16px; }",
		"details.more > summary.icon, .d-head .actions > a.btn.open-mr { width: 44px; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css is missing %q", want)
		}
	}
}

// The fix buttons of the blocker block are compact and on the right (#575), while the shared .mr-actions keeps
// stretching the «Run AI review» + Skip pair across the panel. The page has no style tests: this holds the source.
func TestBoardBlockerFixButtonCompact(t *testing.T) {
	css := string(style)
	for _, want := range []string{
		".tcard.blocker .mr-actions { justify-content: flex-end; margin-top: 0; flex: none; }",
		".tcard.blocker .mr-actions button { flex: none; height: 28px; padding: 0 12px; white-space: nowrap; }",
		// The shared rules stay as they were: the review pair still shares the width.
		".mr-actions button { flex: 1; height: 32px; padding: 0 10px; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
	if strings.Contains(string(board), "the block's full width") {
		t.Error("board.html still says the fix button is the block's full width")
	}
}

// A blocker's reason and its fix button stand on one row (#583): the text, with the file list under it, on the left in
// .blk-text, the button on the right, for the conflicts, CI, comments and the reviewer's Fix alike. Without a button
// (no live agent) the reason stays the bare text. The page has no script or style tests: this holds the source.
func TestBoardBlockerRowsOneLine(t *testing.T) {
	src := string(board)
	i := strings.Index(src, "function blockersHTML(")
	j := strings.Index(src[i:], "\n}\n")
	if i < 0 || j < 0 {
		t.Fatal("no blockersHTML in board.html")
	}
	body := src[i : i+j]
	for _, want := range []string{
		`const blkRow = (text, btn) => btn ? ` + "`" + `<div class="blk-row"><div class="blk-text">${text}</div>${btn}</div>` + "`" + ` : text;`,
		`details.push(blkRow(`,
		`<div class="mono-list">${files.map(esc).join("<br>")}</div>` + "`" + ` : ""),`,
		`askBtn("conflicts", "Resolve conflicts"`,
		`askBtn("ci", "Fix CI"`,
		`askBtn("comments", "Fix comments"`,
		`lines.push(blkRow(`,
		`r.reviewStep === "notes" ? ` + "`" + `<div class="mr-actions">${reviewFixBtn(true, n)}</div>` + "`" + ` : ""));`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("blockersHTML is missing %q", want)
		}
	}
	if n := strings.Count(body, "blkRow("); n != 4 {
		t.Errorf("blkRow wraps %d rows in blockersHTML, want 4 (conflicts, CI, comments, reviewer)", n)
	}
	css := string(style)
	for _, want := range []string{
		".tcard.blocker .blk-row { display: flex; align-items: center; gap: 12px; }",
		".tcard.blocker .blk-text { flex: 1; min-width: 0; display: grid; gap: 8px; overflow-wrap: anywhere; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css lacks %q", want)
		}
	}
}

// After the merge is confirmed, the Merge button stays disabled through the redraws (the polling, loadWork) until
// the merge fails. The board has no script tests, so this checks the page's source for each piece of that.
func TestBoardMergeHeldAfterConfirm(t *testing.T) {
	src, css := string(board), string(style)
	for _, want := range []string{
		// The handler marks the merge as asked before the request and disables the button.
		`{ markAsked(r.id, "merge"); btn.disabled = true; btn.classList.add("busy"); mrAction(r.id, "merge"); }`,
		// The main button and the MR row's icon are drawn disabled while it is held.
		`isAsked(r.id, "merge") ? ` + "`" + `<button class="merge-go busy" disabled title="Merging…">`,
		`merging = isAsked(r.id, "merge")`,
		`!c.mergeable || held || merging`,
		// An error frees the button again and shows the toast, for the merge as well as for ask.
		`if (action === "ask" || action === "merge") asked.delete(`,
		`action === "ask" ? kind : action}`,
		// A slow merge keeps the button disabled past the 2-minute hold while the request is in flight.
		`if (kind === "merge" && merging.has(id)) return true;`,
		`if (action === "merge") merging.add(id);`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("board.html lacks %q", want)
		}
	}
	if strings.Contains(src, `if (failed) { if (action === "ask")`) {
		t.Error("mrAction shows the error toast for ask only: a failed merge would be silent")
	}
	if !strings.Contains(css, ".merge-go.busy") || !strings.Contains(css, ".icon[data-merge].busy") {
		t.Error("style.css has no .busy style for the merge button and icon")
	}
	// The disabled «Merging…» button is enough: the icon must not spin.
	if strings.Contains(css, ".merge-go.busy svg") || strings.Contains(css, ".icon[data-merge].busy svg") {
		t.Error("style.css animates the icon of a busy merge button: it must stay still")
	}
}

// The style sheet's braces balance: a missing "}" once swallowed everything below the reduced-motion media block.
func TestStyleBracesBalance(t *testing.T) {
	css := string(style)
	// Comments may hold braces; strip them first.
	for {
		i := strings.Index(css, "/*")
		if i < 0 {
			break
		}
		j := strings.Index(css[i+2:], "*/")
		if j < 0 {
			t.Fatal("style.css has an unclosed comment")
		}
		css = css[:i] + css[i+2+j+2:]
	}
	if o, c := strings.Count(css, "{"), strings.Count(css, "}"); o != c {
		t.Errorf("style.css has %d \"{\" and %d \"}\"", o, c)
	}
}

// «Continue» is among the hints after the agent's turn (#582): a quick message, placed after the actions that follow
// the task's state and before «What else could break?», so it is the main one only when nothing else is offered.
// The board has no script tests, so this holds the page's source.
func TestBoardNextActionsContinue(t *testing.T) {
	src := string(board)
	i := strings.Index(src, "function nextActions(")
	j := strings.Index(src[i:], "\n}\n")
	if i < 0 || j < 0 {
		t.Fatal("no nextActions in board.html")
	}
	body := src[i : i+j]
	cont := strings.Index(body, `{ label: "Continue", quick: "Continue where you left off." }`)
	if cont < 0 {
		t.Fatal(`nextActions has no «Continue» hint with the quick message "Continue where you left off."`)
	}
	if diff := strings.Index(body, `label: "Show the diff"`); diff < 0 || diff > cont {
		t.Error("«Continue» must come after the actions by the task's state")
	}
	if more := strings.Index(body, `label: "What else could break?"`); more < cont {
		t.Error("«Continue» must come before «What else could break?»")
	}
	// The hints are hidden while the agent works or waits, so «Continue» is never offered then.
	k := strings.Index(src, "function drawNextActions(")
	l := strings.Index(src[k:], "\n}\n")
	if k < 0 || l < 0 {
		t.Fatal("no drawNextActions in board.html")
	}
	if !strings.Contains(src[k:k+l], `box.hidden = s.state === "working" || s.state === "waiting"`) {
		t.Error("the hints are no longer hidden while the agent works or waits")
	}
}
