package trackers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// client dials for dialTimeout at most: a host behind a VPN that is off, whose packets vanish, is told from a slow
// answer by its dial failing (see hostBreakers); the request as a whole may take 30 s.
var client = &http.Client{Timeout: 30 * time.Second, Transport: dialingTransport()}

const dialTimeout = 10 * time.Second

func dialingTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	return t
}

// HTTPError keeps the status, so callers can tell «deleted» (404) from a failure.
type HTTPError struct {
	Status int
	Body   string
	Msg    string
	// RateLimited: the service turned the request away for its rate limit (429, or GitHub's 403 with
	// X-RateLimit-Remaining: 0 or a secondary limit), not for the token's permissions.
	RateLimited bool
}

func (e *HTTPError) Error() string { return e.Msg }

// A request the service turned away for a while (a rate limit, 502–504) is tried again up to retries more times:
// after its Retry-After, else after RetryBase, twice as long each time. A longer wait than retryMax, or one past
// the context's deadline, is not waited for: the error goes back at once.
const retries = 2

var (
	RetryBase = time.Second // tests shorten it
	retryMax  = 20 * time.Second
)

// call sends a JSON request and decodes a JSON answer into out (when not nil); explain turns 401/403/404
// into a message that says what to fix.
func call(ctx context.Context, method, rawURL string, headers map[string]string, body, out any, explain func(status int, body string) string) error {
	var payload []byte
	contentType := ""
	switch b := body.(type) {
	case nil:
	case *multipartBody:
		payload, contentType = b.data, b.contentType
	default:
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload, contentType = data, "application/json"
	}
	host := hostOf(rawURL)
	if err := breakers.closed(host); err != nil {
		return err
	}
	key := ""
	if _, text := out.(*string); method == "GET" && !text { // a job's log is hardly ever asked again unchanged
		key = etagKey(rawURL, headers)
	}
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		cached, haveCached := etags.get(key)
		if haveCached {
			req.Header.Set("If-None-Match", cached.tag)
		}
		res, err := client.Do(req)
		if err != nil {
			if ctx.Err() == nil && noConnection(err) {
				breakers.failed(host, err)
			}
			return err
		}
		data, _ := io.ReadAll(io.LimitReader(res.Body, 20<<20))
		res.Body.Close()
		breakers.answered(host)
		switch {
		case res.StatusCode == http.StatusNotModified && haveCached:
			data = cached.body // a conditional request: GitHub does not count it against the rate limit
		case res.StatusCode >= 300:
			text := strings.TrimSpace(string(data))
			limited := rateLimited(res, text)
			again := limited || (res.StatusCode >= 500 && res.StatusCode <= 504 && idempotent(method))
			if wait := retryWait(res.Header, attempt); again && attempt < retries && wait <= retryMax && sleep(ctx, wait) {
				continue
			}
			msg := fmt.Sprintf("%s %s → %d: %s", method, rawURL, res.StatusCode, clip(text, 300))
			switch {
			case limited:
				msg = "the service's rate limit is exhausted" + resetsAt(res.Header) + " (" + clip(msg, 200) + ")"
			case explain != nil:
				if hint := explain(res.StatusCode, text); hint != "" {
					msg = hint + " (" + clip(msg, 200) + ")"
				}
			}
			return &HTTPError{Status: res.StatusCode, Body: text, Msg: msg, RateLimited: limited}
		default:
			if tag := res.Header.Get("ETag"); key != "" && tag != "" {
				etags.put(key, tag, data)
			}
		}
		if text, ok := out.(*string); ok { // a plain-text answer: a job's log
			*text = string(data)
			return nil
		}
		if out == nil || len(bytes.TrimSpace(data)) == 0 {
			return nil
		}
		return json.Unmarshal(data, out)
	}
}

// idempotent: a method whose request may be sent again without doing twice what it does. A POST is tried again
// only after a rate limit, which turned it away before it was done.
func idempotent(method string) bool {
	return method == "GET" || method == "HEAD" || method == "PUT" || method == "DELETE"
}

// rateLimited: 429, or GitHub's 403 for its rate limit: X-RateLimit-Remaining: 0, a Retry-After (the secondary
// limit) or the words in the answer.
func rateLimited(res *http.Response, text string) bool {
	if res.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if res.StatusCode != http.StatusForbidden {
		return false
	}
	return res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != "" ||
		strings.Contains(strings.ToLower(text), "rate limit")
}

// retryWait: how long to wait before the next try: Retry-After (seconds or a date), else the rate limit's reset,
// else RetryBase doubled per try.
func retryWait(h http.Header, attempt int) time.Duration {
	if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
		if s, err := strconv.Atoi(ra); err == nil {
			return max(time.Duration(s)*time.Second, 0)
		}
		if at, err := http.ParseTime(ra); err == nil {
			return max(time.Until(at), 0)
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return max(time.Until(time.Unix(reset, 0)), 0)
		}
	}
	return RetryBase << attempt
}

// resetsAt: « until 15:04» when the answer says when the rate limit resets.
func resetsAt(h http.Header) string {
	if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > 0 {
		return " until " + time.Unix(reset, 0).Format("15:04")
	}
	if ra, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && ra > 0 {
		return fmt.Sprintf(", retry in %d s", ra)
	}
	return ""
}

// sleep waits d unless the context ends first (or would end before d is over): false then.
func sleep(ctx context.Context, d time.Duration) bool {
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// --- the hosts that do not answer

// A host that could not be connected to breakAfter requests in a row (refused, unreachable, not found, a dial that
// timed out) is not asked again for breakFor: a dead host costs one dial timeout per minute, not one per task in
// every round of the dispatcher. A request that timed out after the connection (a big diff, a job's log) says the
// endpoint is slow, not the host dead: it does not count, or two of them would shut out the comments and statuses.
const breakAfter = 2

// noConnection: the request did not get through to the host.
func noConnection(err error) bool {
	var dns *net.DNSError
	var op *net.OpError
	return errors.As(err, &dns) || errors.As(err, &op) && op.Op == "dial"
}

var breakFor = time.Minute

// HostDown is the answer for a host that is given a rest; Err is its last failure.
type HostDown struct {
	Host  string
	Until time.Time
	Err   error
}

func (e *HostDown) Error() string {
	return fmt.Sprintf("%s does not answer, Mako asks it again after %s: %v", e.Host, e.Until.Format("15:04:05"), e.Err)
}

func (e *HostDown) Unwrap() error { return e.Err }

type hostBreaker struct {
	fails int
	until time.Time
	last  error
}

type hostBreakers struct {
	mu    sync.Mutex
	hosts map[string]*hostBreaker
}

var breakers = &hostBreakers{hosts: map[string]*hostBreaker{}}

// closed: the host's rest as an error, nil when it may be asked.
func (b *hostBreakers) closed(host string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if h := b.hosts[host]; h != nil && time.Now().Before(h.until) {
		return &HostDown{Host: host, Until: h.until, Err: h.last}
	}
	return nil
}

func (b *hostBreakers) failed(host string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.hosts[host]
	if h == nil {
		h = &hostBreaker{}
		b.hosts[host] = h
	}
	h.fails++
	h.last = err
	if h.fails >= breakAfter {
		h.until = time.Now().Add(breakFor)
	}
}

// answered: the host answered (whatever it said): it is up.
func (b *hostBreakers) answered(host string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.hosts, host)
}

// --- conditional requests

// etags keeps the last answer to a GET with its ETag: the next same GET sends If-None-Match, and a 304 gives
// the kept answer back. GitHub does not count a 304 against the rate limit.
var etags = &etagCache{items: map[string]etagItem{}}

// etagsMax answers, etagBodyMax bytes for one, etagBytesMax bytes for all: at most 32 MB are kept.
const etagsMax, etagBodyMax, etagBytesMax = 512, 1 << 20, 32 << 20

type etagItem struct {
	tag  string
	body []byte
}

type etagCache struct {
	mu    sync.Mutex
	items map[string]etagItem
	bytes int // the bodies kept
}

// etagKey: the URL and the credentials (another token may see other things).
func etagKey(rawURL string, headers map[string]string) string {
	h := sha256.New()
	io.WriteString(h, rawURL)
	for _, k := range []string{"Authorization", "PRIVATE-TOKEN"} {
		io.WriteString(h, "\x00"+headers[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *etagCache) get(key string) (etagItem, bool) {
	if key == "" {
		return etagItem{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	return it, ok
}

func (c *etagCache) put(key, tag string, body []byte) {
	if len(body) > etagBodyMax {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.items[key]; ok {
		c.bytes -= len(old.body)
		delete(c.items, key)
	}
	if len(c.items) >= etagsMax || c.bytes+len(body) > etagBytesMax {
		c.items, c.bytes = map[string]etagItem{}, 0 // full: begun anew rather than tracking which is oldest
	}
	c.items[key] = etagItem{tag: tag, body: body}
	c.bytes += len(body)
}

// multipartBody is a form with one file, for call: GitLab's uploads.
type multipartBody struct {
	data        []byte
	contentType string
}

func fileForm(field, name string, data []byte) (*multipartBody, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	f, err := w.CreateFormFile(field, name)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return &multipartBody{data: buf.Bytes(), contentType: w.FormDataContentType()}, nil
}

// perPage and maxPages bound a list read page by page: up to 1000 issues per query.
const perPage, maxPages = 100, 10

// allPages reads a GitHub or GitLab list page by page (page=1, 2, …) until a page comes back short:
// one request returns at most perPage items, the rest of the issues are on the next pages.
func allPages[T any](q url.Values, get func(query string, out *[]T) error) ([]T, error) {
	q.Set("per_page", strconv.Itoa(perPage))
	var all []T
	for page := 1; page <= maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var items []T
		if err := get(q.Encode(), &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < perPage {
			break
		}
	}
	return all, nil
}

func isStatus(err error, status int) bool {
	he, ok := err.(*HTTPError)
	return ok && he.Status == status
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// jsonRaw holds a part of an answer to decode later (GraphQL's data).
type jsonRaw []byte

func (r *jsonRaw) UnmarshalJSON(b []byte) error {
	*r = append((*r)[:0], b...)
	return nil
}

func (r jsonRaw) decode(out any) error {
	if out == nil || len(r) == 0 || string(r) == "null" {
		return nil
	}
	return json.Unmarshal(r, out)
}
