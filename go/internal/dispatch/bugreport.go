package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ReportBug sends a bug found in Mako to the incoming webhook of the config's "bugReportWebhook" (Slack,
// Mattermost, Discord: the body has both "text" and "content"). The user needs no tracker token.
func (d *Dispatcher) ReportBug(ctx context.Context, title, description, version string) error {
	title, description = strings.TrimSpace(title), strings.TrimSpace(description)
	if title == "" {
		return fmt.Errorf("enter a title")
	}
	if description == "" {
		return fmt.Errorf("describe the bug: what you did and what happened")
	}
	hook := d.Config().BugReportWebhook
	if hook == "" {
		return fmt.Errorf("bugReportWebhook is not set in the config")
	}
	if u, err := url.Parse(hook); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("bugReportWebhook is not an http(s) URL")
	}
	text := "*Bug in Mako: " + title + "*\n\n" + description
	if version != "" {
		text += "\n\n_Mako " + version + "_"
	}
	body, _ := json.Marshal(map[string]string{"text": text, "content": text})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", hook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not send the report: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("the webhook answered %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
