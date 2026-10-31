package trackers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 30 * time.Second}

// HTTPError keeps the status, so callers can tell «deleted» (404) from a failure.
type HTTPError struct {
	Status int
	Body   string
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }

// call sends a JSON request and decodes a JSON answer into out (when not nil); explain turns 401/403/404
// into a message that says what to fix.
func call(ctx context.Context, method, url string, headers map[string]string, body, out any, explain func(status int, body string) string) error {
	var rd io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case *multipartBody:
		rd, contentType = bytes.NewReader(b.data), b.contentType
	default:
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd, contentType = bytes.NewReader(data), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
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
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 20<<20))
	if res.StatusCode >= 300 {
		text := strings.TrimSpace(string(data))
		msg := fmt.Sprintf("%s %s → %d: %s", method, url, res.StatusCode, clip(text, 300))
		if explain != nil {
			if hint := explain(res.StatusCode, text); hint != "" {
				msg = hint + " (" + clip(msg, 200) + ")"
			}
		}
		return &HTTPError{Status: res.StatusCode, Body: text, Msg: msg}
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
