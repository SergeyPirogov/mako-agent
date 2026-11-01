package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/dispatch"
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
		want := map[string]string{"grp/app#2": "WIP-ліміт: 1 з 1 в роботі"}[r.ID]
		if r.QueueWhy != want || r.Starting != nil {
			t.Fatalf("%s: queueWhy %q, starting %+v", r.ID, r.QueueWhy, r.Starting)
		}
	}
	res, _ = http.Get(srv.URL + "/")
	if page, _ := io.ReadAll(res.Body); !strings.Contains(string(page), `starting: "запускаю…"`) {
		t.Fatal("no «запускаю» on the board")
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
	if res := get("/pair?code=guess"); res.StatusCode != 401 {
		t.Fatalf("a made-up code paired: %d", res.StatusCode)
	}
	// The QR's code pairs the phone once.
	res := get("/pair?code=" + phones.NewPairing())
	if res.StatusCode != 302 {
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
	res, _ := http.Get(srv.URL + "/api/state?seen=1")
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
	res, err := http.Post(srv.URL+"/api/answers?id="+url.QueryEscape(task.ID), "application/json", strings.NewReader(`{"answers":[{"q":"Яка кнопка?","a":"«Оплатити»"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	json.NewDecoder(res.Body).Decode(&reply)
	if reply.OK || !strings.Contains(reply.Error, "трекер gone") {
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
	res, err := http.Post(srv.URL+"/api/answers?id="+url.QueryEscape("grp/app#4"), "application/json", strings.NewReader(`{"answers":["старий формат"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	json.NewDecoder(res.Body).Decode(&reply)
	if reply.OK || !strings.Contains(reply.Error, "не вдалось прочитати відповіді") {
		t.Fatalf("reply %+v", reply)
	}
}
