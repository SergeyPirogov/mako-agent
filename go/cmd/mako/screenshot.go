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

const screenshotUsage = `mako screenshot: скриншот інтерфейсу для задачі, що його змінює.

Без -url: збирає Mako з worktree (або з -rev), запускає його лише для читання на вільному порту з копією даних
поточного Mako і фотографує сторінку -page у headless Chrome. З -url: фотографує цю адресу (локальна збірка
іншого проєкту).

Коли задача відома (-task або MAKO_TASK) і Mako запущений, скриншот іде до нього: Mako показує його в чаті задачі
й завантажує в трекер, а команда друкує Markdown картинки для опису MR.

  mako screenshot -rev origin/main -name before            # «до»: основна гілка
  mako screenshot -name after                              # «після»: цей worktree
  mako screenshot -name after -page /settings
  mako screenshot -name after -url http://127.0.0.1:5173/  # не Mako

`

func screenshot(args []string) error {
	fs := flag.NewFlagSet("screenshot", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), screenshotUsage); fs.PrintDefaults() }
	src := fs.String("src", ".", "worktree з Mako, який фотографувати")
	rev := fs.String("rev", "", "ревізія для «до» (origin/main): збирається в тимчасовому worktree")
	page := fs.String("page", "/", "сторінка Mako: /, /settings, /#<id задачі>")
	target := fs.String("url", "", "фотографувати цю адресу замість зібраного Mako")
	name := fs.String("name", "screenshot", "назва: before, after, …")
	out := fs.String("out", "", "куди зберегти PNG (типово тимчасовий файл)")
	size := fs.String("size", "1440x900", "розмір вікна, ШxВ")
	wait := fs.Duration("wait", 4*time.Second, "скільки чекати, поки сторінка завантажить дані")
	homeFlag := fs.String("home", "", "папка поточного Mako з налаштуваннями й даними")
	task := fs.String("task", os.Getenv("MAKO_TASK"), "задача, до якої скриншот: Mako покаже його в чаті й завантажить у трекер")
	makoURL := fs.String("mako", os.Getenv("MAKO_URL"), "адреса запущеного Mako (типово з його налаштувань)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	var width, height int
	if _, err := fmt.Sscanf(*size, "%dx%d", &width, &height); err != nil || width <= 0 || height <= 0 {
		return fmt.Errorf("-size: %q, а треба ШxВ, як 1440x900", *size)
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
	fmt.Println("Скриншот:", file)
	if *task == "" {
		fmt.Println("Задача не вказана (-task): скриншот лише у файлі.")
		return nil
	}
	if *makoURL == "" {
		*makoURL = runningURL(home)
	}
	md, err := deliver(*makoURL, *task, safeName(*name)+".png", file)
	if err != nil {
		return fmt.Errorf("скриншот у %s, але Mako його не прийняв: %w", file, err)
	}
	fmt.Println("Mako показав його в чаті задачі.")
	if md != "" {
		fmt.Println("Для опису MR:")
		fmt.Println(md)
	}
	return nil
}

// serveMako builds the Mako to photograph and runs it read-only; it returns its address and what to clean up.
func serveMako(ctx context.Context, src, rev, home string) (string, func(), error) {
	top, err := gitOut(src, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, fmt.Errorf("%s не git-репозиторій: %w", src, err)
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
			return "", cleanup, fmt.Errorf("worktree для %s: %w", rev, err)
		}
		undo = append(undo, func() { gitOut(top, "worktree", "remove", "--force", repo) })
	}
	module := shot.Module(repo)
	if module == "" {
		return "", cleanup, fmt.Errorf("у %s немає Mako (cmd/mako): для іншого проєкту запустіть його збірку й вкажіть -url", repo)
	}
	bin := filepath.Join(tmp, "mako")
	if err := shot.Build(ctx, module, bin); err != nil {
		return "", cleanup, err
	}
	data := filepath.Join(tmp, "home")
	if err := shot.CopyHome(home, data); err != nil {
		return "", cleanup, fmt.Errorf("копія даних Mako: %w", err)
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
		fmt.Println("У трекер не завантажено:", reply.Upload, "— додайте файл до MR вручну.")
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
