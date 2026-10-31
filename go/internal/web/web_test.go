package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
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
	d := dispatch.New(&config.Config{Notifications: config.Notifications{SnoozeMin: 60}}, store, nil)
	srv := httptest.NewServer((&Server{D: d, Version: "test"}).Handler())
	defer srv.Close()

	res, _ := http.Get(srv.URL + "/")
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d", res.StatusCode)
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

	res, _ = http.Get(srv.URL + "/api/state")
	var st stateReply
	json.NewDecoder(res.Body).Decode(&st)
	if len(st.Records) != 1 || !st.Records[0].Postponed || st.Records[0].Column != "new" {
		t.Fatalf("state %+v", st.Records)
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

func TestSettingsSaveReloads(t *testing.T) {
	home := t.TempDir()
	store, _ := state.Open(t.TempDir())
	d := dispatch.New(&config.Config{}, store, nil)
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
	d := dispatch.New(&config.Config{}, store, nil)
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
	d := dispatch.New(&config.Config{}, store, nil)
	started := make(chan bool, 1)
	u := &update.Updater{Source: t.TempDir(), Exe: filepath.Join(t.TempDir(), "mako"),
		Test: func(context.Context, string) error { started <- true; return errors.New("no") }}
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
	// Not a repository: the update fails before testing, and says so.
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
