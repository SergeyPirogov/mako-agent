package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/shot"
)

const screenshotUsage = `mako screenshot: screenshot of the interface for a task that changes it.

Without -url: builds Mako from the worktree (or from -rev), runs it read-only on a free port with a copy of the data
of the current Mako and screenshots the -page page in headless Chrome. With -url: screenshots that address (a local build
of another project).

When the task is known (-task or MAKO_TASK) and Mako is running, the screenshot goes to it: Mako shows it in the task chat
and uploads it to the tracker, and the command prints the image Markdown for the MR description.

  mako screenshot -rev origin/main -name before            # "before": the main branch
  mako screenshot -name after                              # "after": this worktree
  mako screenshot -name after -page /settings
  mako screenshot -name after -url http://127.0.0.1:5173/  # not Mako

`

func screenshot(args []string) error {
	fs := flag.NewFlagSet("screenshot", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), screenshotUsage); fs.PrintDefaults() }
	src := fs.String("src", ".", "worktree with Mako to screenshot")
	rev := fs.String("rev", "", "revision for \"before\" (origin/main): built in a temporary worktree")
	page := fs.String("page", "/", "Mako page: /, /settings, /#<task id>")
	target := fs.String("url", "", "screenshot this address instead of a built Mako")
	name := fs.String("name", "screenshot", "name: before, after, …")
	out := fs.String("out", "", "where to save the PNG (default a temporary file)")
	size := fs.String("size", "1440x900", "window size, WxH")
	wait := fs.Duration("wait", 4*time.Second, "how long to wait for the page to load data")
	homeFlag := fs.String("home", "", "folder of the current Mako with settings and data")
	task := fs.String("task", os.Getenv("MAKO_TASK"), "task the screenshot belongs to: Mako shows it in the chat and uploads it to the tracker")
	makoURL := fs.String("mako", os.Getenv("MAKO_URL"), "address of the running Mako (default from its settings)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	var width, height int
	if _, err := fmt.Sscanf(*size, "%dx%d", &width, &height); err != nil || width <= 0 || height <= 0 {
		return fmt.Errorf("-size: %q, expected WxH, like 1440x900", *size)
	}
	chrome, err := shot.FindChrome()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	home := findHome(*homeFlag)

	file := *out
	if file == "" {
		dir, err := os.MkdirTemp("", "mako-shot-")
		if err != nil {
			return err
		}
		file = filepath.Join(dir, safeName(*name)+".png")
	}
	link := *target
	if link == "" {
		base, cleanup, err := serveMako(ctx, *src, *rev, home)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return err
		}
		link = base + "/" + strings.TrimPrefix(*page, "/")
	}
	if err := shot.Capture(ctx, chrome, link, file, width, height, *wait); err != nil {
		return err
	}
	fmt.Println("Screenshot:", file)
	if *task == "" {
		fmt.Println("No task given (-task): the screenshot is only in the file.")
		return nil
	}
	if *makoURL == "" {
		*makoURL = runningURL(home)
	}
	md, err := deliver(*makoURL, *task, safeName(*name)+".png", file)
	if err != nil {
		return fmt.Errorf("screenshot in %s, but Mako did not accept it: %w", file, err)
	}
	fmt.Println("Mako showed it in the task chat.")
	if md != "" {
		fmt.Println("For the MR description:")
		fmt.Println(md)
	}
	return nil
}

// serveMako builds the Mako to photograph and runs it read-only; it returns its address and what to clean up.
func serveMako(ctx context.Context, src, rev, home string) (string, func(), error) {
	top, err := gitOut(src, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, fmt.Errorf("%s is not a git repository: %w", src, err)
	}
	tmp, err := os.MkdirTemp("", "mako-shot-")
	if err != nil {
		return "", nil, err
	}
	var undo []func()
	cleanup := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		os.RemoveAll(tmp)
	}
	repo := top
	if rev != "" {
		repo = filepath.Join(tmp, "src")
		if _, err := gitOut(top, "worktree", "add", "--detach", repo, rev); err != nil {
			return "", cleanup, fmt.Errorf("worktree for %s: %w", rev, err)
		}
		undo = append(undo, func() { gitOut(top, "worktree", "remove", "--force", repo) })
	}
	module := shot.Module(repo)
	if module == "" {
		return "", cleanup, fmt.Errorf("there is no Mako (cmd/mako) in %s: for another project run its build and pass -url", repo)
	}
	bin := filepath.Join(tmp, "mako")
	if err := shot.Build(ctx, module, bin); err != nil {
		return "", cleanup, err
	}
	data := filepath.Join(tmp, "home")
	if err := shot.CopyHome(home, data); err != nil {
		return "", cleanup, fmt.Errorf("copy of Mako data: %w", err)
	}
	port, err := shot.FreePort()
	if err != nil {
		return "", cleanup, err
	}
	stopMako, err := shot.Serve(ctx, bin, data, port)
	if err != nil {
		return "", cleanup, err
	}
	undo = append(undo, stopMako)
	return fmt.Sprintf("http://127.0.0.1:%d", port), cleanup, nil
}

// deliver hands the screenshot to the running Mako for the task: it shows it in the chat and uploads it to the
// tracker; it returns the Markdown for the MR.
func deliver(base, task, name, file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	u := strings.TrimRight(base, "/") + "/api/screenshot?id=" + url.QueryEscape(task) + "&name=" + url.QueryEscape(name)
	c := http.Client{Timeout: 2 * time.Minute}
	res, err := c.Post(u, "image/png", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var reply struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Markdown string `json:"markdown"`
		Upload   string `json:"uploadError"`
	}
	if err := json.NewDecoder(res.Body).Decode(&reply); err != nil {
		return "", fmt.Errorf("%s: %s", u, res.Status)
	}
	if !reply.OK {
		return "", fmt.Errorf("%s", reply.Error)
	}
	if reply.Upload != "" {
		fmt.Println("Not uploaded to the tracker:", reply.Upload, "- add the file to the MR manually.")
	}
	return reply.Markdown, nil
}

// runningURL: where the current Mako's board is, by its settings.
func runningURL(home string) string {
	cfg, _ := config.Load(home)
	return fmt.Sprintf("http://127.0.0.1:%d", cfg.Notifications.Port)
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, strings.TrimSuffix(s, ".png"))
	if s = strings.Trim(s, "-"); s == "" {
		return "screenshot"
	}
	return s
}
