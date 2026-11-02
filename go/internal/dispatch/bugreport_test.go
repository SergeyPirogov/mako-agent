package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

func TestReportBug(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	d := &Dispatcher{Cfg: &config.Config{BugReportWebhook: srv.URL}}
	if err := d.ReportBug(context.Background(), "Crash", "on save", "v1"); err != nil {
		t.Fatal(err)
	}
	if got["text"] != got["content"] || !strings.Contains(got["text"], "Crash") || !strings.Contains(got["text"], "on save") || !strings.Contains(got["text"], "v1") {
		t.Errorf("body %v", got)
	}
	for _, c := range []struct{ title, desc, hook, want string }{
		{"", "x", srv.URL, "title"},
		{"t", " ", srv.URL, "describe"},
		{"t", "x", "", "not set"},
		{"t", "x", "ftp://h", "http(s)"},
	} {
		d := &Dispatcher{Cfg: &config.Config{BugReportWebhook: c.hook}}
		if err := d.ReportBug(context.Background(), c.title, c.desc, ""); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 500) }))
	defer bad.Close()
	d = &Dispatcher{Cfg: &config.Config{BugReportWebhook: bad.URL}}
	if err := d.ReportBug(context.Background(), "t", "x", ""); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("%v", err)
	}
}
