package trackers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
)

func fastRetries(t *testing.T) {
	t.Helper()
	old := RetryBase
	RetryBase = time.Millisecond
	t.Cleanup(func() { RetryBase = old })
}

// 429 and 5xx are tried again (a 5xx only for a request that may be sent twice); when they go on, the error says so.
func TestCallRetries(t *testing.T) {
	fastRetries(t)
	var calls atomic.Int32
	fails, status := 0, 429
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(calls.Add(1)) <= fails {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(status)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	var out struct{ OK bool }

	fails = 2
	if err := call(ctx, "POST", srv.URL, nil, map[string]any{}, &out, nil); err != nil || !out.OK || calls.Load() != 3 {
		t.Fatalf("429 twice: %v %+v, %d calls", err, out, calls.Load())
	}

	calls.Store(0)
	fails, status = 10, 503
	err := call(ctx, "GET", srv.URL, nil, nil, &out, nil)
	if !isStatus(err, 503) || calls.Load() != 1+retries {
		t.Fatalf("503 always: %v, %d calls", err, calls.Load())
	}

	// A POST that met a 5xx may have been done: not sent again.
	calls.Store(0)
	if err := call(ctx, "POST", srv.URL, nil, map[string]any{}, &out, nil); !isStatus(err, 503) || calls.Load() != 1 {
		t.Fatalf("POST 503: %v, %d calls", err, calls.Load())
	}

	// Asked to wait longer than the context lasts: the error comes back at once.
	calls.Store(0)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(429)
	}))
	defer slow.Close()
	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	err = call(short, "GET", slow.URL, nil, nil, &out, nil)
	var he *HTTPError
	if !errors.As(err, &he) || !he.RateLimited || calls.Load() != 1 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("long Retry-After: %v, %d calls in %s", err, calls.Load(), time.Since(start))
	}
}

// GitHub's 403 for its rate limit is not «the token lacks permissions»; its secondary limit is waited out.
func TestCallRateLimit403(t *testing.T) {
	fastRetries(t)
	var calls atomic.Int32
	mode := "primary"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		switch {
		case mode == "primary":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		case mode == "secondary" && n == 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
		case mode == "permissions":
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	g, _ := NewGitHub(&config.Tracker{ID: "github", APIURL: srv.URL}, "tok")
	ctx := context.Background()

	err := g.do(ctx, "GET", "/user", nil, nil)
	var he *HTTPError
	if !errors.As(err, &he) || !he.RateLimited || !strings.Contains(err.Error(), "rate limit is exhausted until") || calls.Load() != 1 {
		t.Fatalf("primary: %v, %d calls", err, calls.Load())
	}

	mode = "secondary"
	calls.Store(0)
	if err := g.do(ctx, "GET", "/user", nil, nil); err != nil || calls.Load() != 2 {
		t.Fatalf("secondary: %v, %d calls", err, calls.Load())
	}

	mode = "permissions"
	calls.Store(0)
	err = g.do(ctx, "GET", "/user", nil, nil)
	if !errors.As(err, &he) || he.RateLimited || !strings.Contains(err.Error(), "lacks permissions") || calls.Load() != 1 {
		t.Fatalf("permissions: %v, %d calls", err, calls.Load())
	}
}

// A GET answered with an ETag is asked again with If-None-Match; its 304 gives the kept answer.
func TestCallETag(t *testing.T) {
	var full, notModified atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(`{"n":7}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		var out struct{ N int }
		if err := call(ctx, "GET", srv.URL+"/x", map[string]string{"Authorization": "Bearer a"}, nil, &out, nil); err != nil || out.N != 7 {
			t.Fatalf("try %d: %v %+v", i, err, out)
		}
	}
	if full.Load() != 1 || notModified.Load() != 1 {
		t.Fatalf("full %d, 304 %d", full.Load(), notModified.Load())
	}
	// Another token does not get the first one's answer.
	var out struct{ N int }
	if err := call(ctx, "GET", srv.URL+"/x", map[string]string{"Authorization": "Bearer b"}, nil, &out, nil); err != nil || full.Load() != 2 {
		t.Fatalf("other token: %v, full %d", err, full.Load())
	}
}

// A host that does not answer twice in a row is not dialled for a while: the error says so at once and keeps the
// cause; once the rest is over it is asked again, and an answer makes it up again.
func TestHostBreaker(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	ctx := context.Background()
	old := breakFor
	breakFor = 50 * time.Millisecond
	t.Cleanup(func() { breakFor = old })

	for i := 0; i < breakAfter; i++ {
		if err := call(ctx, "GET", "http://"+addr+"/x", nil, nil, nil, nil); err == nil || errors.As(err, new(*HostDown)) {
			t.Fatalf("try %d: %v", i, err)
		}
	}
	err := call(ctx, "GET", "http://"+addr+"/y", nil, nil, nil, nil)
	var down *HostDown
	if !errors.As(err, &down) || down.Host != addr || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("resting: %v", err)
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Fatalf("the cause is lost: %v", err)
	}

	time.Sleep(60 * time.Millisecond)
	l, err = net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("the port is taken meanwhile: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	defer srv.Close()
	if err := call(ctx, "GET", "http://"+addr+"/z", nil, nil, nil, nil); err != nil {
		t.Fatalf("after the rest: %v", err)
	}
	if err := breakers.closed(addr); err != nil {
		t.Fatalf("up again: %v", err)
	}
	// A cancelled request says nothing of the host.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for i := 0; i < breakAfter+1; i++ {
		call(cancelled, "GET", "http://"+addr+"/z", nil, nil, nil, nil)
	}
	if err := breakers.closed(addr); err != nil {
		t.Fatalf("cancelled requests rested the host: %v", err)
	}
}

func TestRetryWait(t *testing.T) {
	h := http.Header{}
	if retryWait(h, 0) != RetryBase || retryWait(h, 2) != 4*RetryBase {
		t.Errorf("backoff %s %s", retryWait(h, 0), retryWait(h, 2))
	}
	h.Set("Retry-After", "3")
	if retryWait(h, 1) != 3*time.Second {
		t.Errorf("seconds %s", retryWait(h, 1))
	}
	h.Set("Retry-After", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat))
	if retryWait(h, 0) != 0 {
		t.Errorf("a date gone by %s", retryWait(h, 0))
	}
	h = http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}}
	if w := retryWait(h, 0); w < 59*time.Minute {
		t.Errorf("reset %s", w)
	}
}

// allPages reads until a short page, and no more than maxPages; a failed page fails the whole list.
func TestAllPagesBounds(t *testing.T) {
	read := func(total int, failAt int) (int, []string, error) {
		var pages []string
		items, err := allPages(url.Values{"sort": {"asc"}}, func(query string, out *[]int) error {
			q, _ := url.ParseQuery(query)
			if q.Get("sort") != "asc" || q.Get("per_page") != strconv.Itoa(perPage) {
				t.Errorf("query %s", query)
			}
			page, _ := strconv.Atoi(q.Get("page"))
			pages = append(pages, q.Get("page"))
			if page == failAt {
				return fmt.Errorf("page %d failed", page)
			}
			for n := (page-1)*perPage + 1; n <= min(page*perPage, total); n++ {
				*out = append(*out, n)
			}
			return nil
		})
		return len(items), pages, err
	}
	if n, pages, err := read(perPage, 0); err != nil || n != perPage || strings.Join(pages, ",") != "1,2" {
		t.Errorf("exactly a page: %d %v %v", n, pages, err)
	}
	if n, pages, err := read(perPage-1, 0); err != nil || n != perPage-1 || len(pages) != 1 {
		t.Errorf("short page: %d %v %v", n, pages, err)
	}
	if n, pages, err := read(perPage*maxPages+5, 0); err != nil || n != perPage*maxPages || len(pages) != maxPages {
		t.Errorf("past the cap: %d %d pages %v", n, len(pages), err)
	}
	if n, _, err := read(perPage*3, 2); err == nil || n != 0 {
		t.Errorf("a failed page: %d %v", n, err)
	}
}

// The ETags kept never weigh more than etagBytesMax, and a text answer (a job's log) is not kept at all.
func TestETagCacheBounds(t *testing.T) {
	c := &etagCache{items: map[string]etagItem{}}
	body := make([]byte, etagBodyMax)
	for i := 0; i < etagBytesMax/etagBodyMax; i++ {
		c.put(fmt.Sprint(i), "t", body)
	}
	if len(c.items) != etagBytesMax/etagBodyMax || c.bytes != etagBytesMax {
		t.Fatalf("full: %d answers, %d bytes", len(c.items), c.bytes)
	}
	c.put("one more", "t", body)
	if len(c.items) != 1 || c.bytes != etagBodyMax {
		t.Fatalf("over the limit: %d answers, %d bytes", len(c.items), c.bytes)
	}
	// The same URL again replaces its answer; a too big one is not kept.
	c.put("one more", "t2", body[:10])
	c.put("big", "t", make([]byte, etagBodyMax+1))
	if it, _ := c.get("one more"); len(c.items) != 1 || c.bytes != 10 || it.tag != "t2" {
		t.Fatalf("replaced: %d answers, %d bytes, %+v", len(c.items), c.bytes, it)
	}

	var full atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			t.Error("a text answer was asked again conditionally")
		}
		full.Add(1)
		w.Header().Set("ETag", `"log"`)
		w.Write([]byte("line"))
	}))
	defer srv.Close()
	for i := 0; i < 2; i++ {
		var log string
		if err := call(context.Background(), "GET", srv.URL+"/log", nil, nil, &log, nil); err != nil || log != "line" {
			t.Fatalf("%v %q", err, log)
		}
	}
	if full.Load() != 2 {
		t.Fatalf("asked %d times", full.Load())
	}
}

// A request that timed out on a slow endpoint does not shut the host: only a failed connection counts.
func TestSlowAnswerDoesNotRestHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	old := client
	client = &http.Client{Timeout: 50 * time.Millisecond}
	t.Cleanup(func() { client = old })
	host := hostOf(srv.URL)
	for i := 0; i < breakAfter+2; i++ {
		if err := call(context.Background(), "GET", srv.URL+"/slow", nil, nil, nil, nil); err == nil || errors.As(err, new(*HostDown)) {
			t.Fatalf("try %d: %v", i, err)
		}
	}
	if err := breakers.closed(host); err != nil {
		t.Fatalf("the host rests after slow answers: %v", err)
	}
	if err := call(context.Background(), "GET", srv.URL+"/fast", nil, nil, nil, nil); err != nil {
		t.Fatalf("a quick request after the slow ones: %v", err)
	}
}
