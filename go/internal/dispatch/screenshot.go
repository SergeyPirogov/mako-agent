package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/chat"
)

// --- screenshots of a UI task: `mako screenshot` hands them over for the chat and the MR

var unsafeFile = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeFile(s string) string { return strings.Trim(unsafeFile.ReplaceAllString(s, "-"), "-.") }

// ScreenshotDir: where the task's screenshots are kept for the board.
func (d *Dispatcher) ScreenshotDir(id string) string {
	return filepath.Join(d.Config().StateDir, "screenshots", safeFile(id))
}

// Screenshot keeps a screenshot the task's agent took, shows it in the task's chat and uploads it to the
// task's tracker. It returns the board's link to it and the Markdown for the MR; uploadErr says why there is no
// Markdown, while the screenshot is still kept and shown.
func (d *Dispatcher) Screenshot(ctx context.Context, id, name string, data []byte) (local, markdown string, uploadErr error, err error) {
	r, ok := d.Store.Get(id)
	if !ok {
		return "", "", nil, fmt.Errorf("no task %q", id)
	}
	if name = safeFile(name); name == "" {
		name = "screenshot.png"
	}
	_, local, err = d.keepImage(id, time.Now().Format("20060102-150405")+"-"+name, data)
	if err != nil {
		return "", "", nil, err
	}
	if s := d.Chats.Get(id); s != nil {
		s.Show(chat.Item{Kind: "image", Text: local, Output: name})
	}
	d.Log(id, "screenshot "+name)
	_, c, err := d.creator(r.Task.TrackerID)
	if err != nil {
		return local, "", err, nil
	}
	markdown, uploadErr = c.Upload(ctx, r.Task.Project, name, data)
	return local, markdown, uploadErr, nil
}

// keepImage saves a picture of the task's under file: its path on disk and the board's link to it.
func (d *Dispatcher) keepImage(id, file string, data []byte) (path, local string, err error) {
	dir := d.ScreenshotDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, file)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", "", err
	}
	return path, "/api/screenshots/" + safeFile(id) + "/" + file, nil
}

// Attach keeps a picture you attach to a message for the task's agent: the message names its path (chat.ImageLine)
// and the agent reads it from there.
func (d *Dispatcher) Attach(id, name string, data []byte) (path, local string, err error) {
	if _, ok := d.Store.Get(id); !ok && !d.isFreeChat(id) {
		return "", "", fmt.Errorf("no task %q", id)
	}
	if name = safeFile(name); name == "" {
		name = "image.png"
	}
	return d.keepImage(id, time.Now().Format("20060102-150405.000")+"-"+name, data)
}
