package orca

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"
)

// Orca shows only the first 200 characters of a chat agent's last message, on one line. Claude Code keeps the
// whole conversation in its transcript: ~/.claude/projects/<the worktree's path, dashed>/<session>.jsonl, or
// under Orca's own Claude profiles. LastMessage reads the last thing the agent said from there.

var notAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// claudeProjectDirs: where Claude Code may keep the transcripts of sessions started in worktree.
func claudeProjectDirs(worktree string) []string {
	home, _ := os.UserHomeDir()
	roots := []string{}
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		roots = append(roots, filepath.Join(dir, "projects"))
	}
	roots = append(roots, filepath.Join(home, ".claude", "projects"))
	orcaData := filepath.Join(home, ".config", "orca")
	if runtime.GOOS == "darwin" {
		orcaData = filepath.Join(home, "Library", "Application Support", "orca")
	}
	if profiles, err := os.ReadDir(filepath.Join(orcaData, "claude-profiles")); err == nil {
		for _, p := range profiles {
			roots = append(roots, filepath.Join(orcaData, "claude-profiles", p.Name(), "home", "projects"))
		}
	}
	path := strings.TrimRight(worktree, "/")
	names := []string{notAlnum.ReplaceAllString(path, "-")}
	if nfc := notAlnum.ReplaceAllString(norm.NFC.String(path), "-"); nfc != names[0] {
		names = append(names, nfc)
	}
	var dirs []string
	for _, root := range roots {
		for _, n := range names {
			dirs = append(dirs, filepath.Join(root, n))
		}
	}
	return dirs
}

type transcriptCache struct {
	mod  time.Time
	size int64
	text string
}

var (
	cacheMu sync.Mutex
	cache   = map[string]transcriptCache{}
)

// LatestTranscript: the newest Claude Code session started in worktree, "" when there is none.
func LatestTranscript(worktree string) string {
	newest, _ := newestSession(worktree)
	return newest
}

// Transcript: the transcript of Claude Code's session sessionID in worktree, else the newest one there.
func Transcript(worktree, sessionID string) string {
	if p := sessionFile(worktree, sessionID); p != "" {
		return p
	}
	return LatestTranscript(worktree)
}

// HasSession: Claude Code kept the session sessionID of worktree, so `claude --resume` has it to go on.
func HasSession(worktree, sessionID string) bool { return sessionFile(worktree, sessionID) != "" }

// SessionFile: the transcript of Claude Code's session sessionID in worktree, "" when it is not kept.
func SessionFile(worktree, sessionID string) string { return sessionFile(worktree, sessionID) }

func sessionFile(worktree, sessionID string) string {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\.`) {
		return ""
	}
	for _, dir := range claudeProjectDirs(worktree) {
		if p := filepath.Join(dir, sessionID+".jsonl"); fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// HasTranscript: Claude Code kept a session started in worktree, so `claude --continue` there has one to go on.
func HasTranscript(worktree string) bool {
	for _, dir := range claudeProjectDirs(worktree) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".jsonl") {
				return true
			}
		}
	}
	return false
}

// LastMessage: the agent's last message in its newest Claude Code session in worktree, "" when there is none.
func LastMessage(worktree string) string {
	newest, newestInfo := newestSession(worktree)
	if newest == "" {
		return ""
	}
	cacheMu.Lock()
	c, ok := cache[newest]
	cacheMu.Unlock()
	if ok && c.mod.Equal(newestInfo.ModTime()) && c.size == newestInfo.Size() {
		return c.text
	}
	text := lastAssistantText(newest, newestInfo.Size())
	cacheMu.Lock()
	cache[newest] = transcriptCache{mod: newestInfo.ModTime(), size: newestInfo.Size(), text: text}
	cacheMu.Unlock()
	return text
}

// lastAssistantText reads the end of a transcript (a session can be many MB) and joins the text blocks of the
// last assistant message of the main conversation (not a subagent's).
func lastAssistantText(path string, size int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 4 << 20
	if size > tail {
		f.Seek(size-tail, io.SeekStart)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	if size > tail {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	type entry struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			ID      string          `json:"id"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	var lines [][]byte
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 1<<20), 64<<20)
	for s.Scan() {
		lines = append(lines, append([]byte{}, s.Bytes()...))
	}
	id := ""
	var parts []string
	for i := len(lines) - 1; i >= 0; i-- {
		var e entry
		if json.Unmarshal(lines[i], &e) != nil || e.Type != "assistant" || e.IsSidechain || e.Message.Role != "assistant" {
			continue
		}
		if id != "" && e.Message.ID != id {
			break // an earlier message
		}
		texts := textBlocks(e.Message.Content)
		if id == "" {
			if len(texts) == 0 {
				continue // a tool call: the words come in an earlier part or message
			}
			id = e.Message.ID
		}
		parts = append(texts, parts...)
		if id == "" {
			break
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func textBlocks(raw json.RawMessage) []string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &blocks)
	var out []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

// Turn is one step of a conversation as the board shows it.
type Turn struct {
	Role string `json:"role"` // "user", "agent", "tool"
	Text string `json:"text"`
	At   string `json:"at,omitempty"`
}

type conversationCache struct {
	mod   time.Time
	size  int64
	turns []Turn
	used  uint64 // convTick at the last use, for eviction
}

// convCacheMax bounds how many transcripts stay parsed in memory.
const convCacheMax = 64

var (
	convMu    sync.Mutex
	convCache = map[string]conversationCache{}
	convTick  uint64
)

// storeConv puts c in the cache, evicting the least recently used entries beyond convCacheMax.
// The caller holds convMu.
func storeConv(path string, c conversationCache) {
	convTick++
	c.used = convTick
	convCache[path] = c
	for len(convCache) > convCacheMax {
		oldest, first := "", true
		var oldestUsed uint64
		for k, v := range convCache {
			if first || v.used < oldestUsed {
				oldest, oldestUsed, first = k, v.used, false
			}
		}
		delete(convCache, oldest)
	}
}

// Transcripts: the transcripts of the sessions started in worktree, the oldest first; not the subagents' ones.
func Transcripts(worktree string) []string {
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	for _, dir := range claudeProjectDirs(worktree) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if info, err := e.Info(); err == nil && !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				files = append(files, file{filepath.Join(dir, e.Name()), info.ModTime()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	return out
}

// newestSession: the newest Claude Code transcript of a session started in worktree.
func newestSession(worktree string) (string, os.FileInfo) {
	var newest string
	var newestInfo os.FileInfo
	for _, dir := range claudeProjectDirs(worktree) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			if info, err := e.Info(); err == nil && (newestInfo == nil || info.ModTime().After(newestInfo.ModTime())) {
				newest, newestInfo = filepath.Join(dir, e.Name()), info
			}
		}
	}
	return newest, newestInfo
}

// Conversation: the agent's newest Claude Code session in worktree, as the user's messages, the agent's words
// and one line per tool it used; the last limit turns.
func Conversation(worktree string, limit int) []Turn {
	path, info := newestSession(worktree)
	if path == "" {
		return nil
	}
	convMu.Lock()
	c, ok := convCache[path]
	if ok {
		storeConv(path, c)
	}
	convMu.Unlock()
	if !ok || !c.mod.Equal(info.ModTime()) || c.size != info.Size() {
		c = conversationCache{mod: info.ModTime(), size: info.Size(), turns: readConversation(path, info.Size())}
		convMu.Lock()
		storeConv(path, c)
		convMu.Unlock()
	}
	if len(c.turns) > limit {
		return c.turns[len(c.turns)-limit:]
	}
	return c.turns
}

// Lines Claude Code adds to the user's side that the user did not write.
var notTyped = regexp.MustCompile(`^\s*<(command-|local-command|system-reminder|user-prompt-submit-hook|bash-)`)

func readConversation(path string, size int64) []Turn {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const tail = 8 << 20
	if size > tail {
		f.Seek(size-tail, io.SeekStart)
	}
	data, _ := io.ReadAll(f)
	if size > tail {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	var turns []Turn
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 1<<20), 64<<20)
	for s.Scan() {
		var e struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			IsMeta      bool   `json:"isMeta"`
			Timestamp   string `json:"timestamp"`
			Message     struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(s.Bytes(), &e) != nil || e.IsSidechain || e.IsMeta {
			continue
		}
		switch e.Type {
		case "user":
			for _, t := range textBlocks(e.Message.Content) {
				if !notTyped.MatchString(t) {
					turns = append(turns, Turn{Role: "user", Text: strings.TrimSpace(t), At: e.Timestamp})
				}
			}
		case "assistant":
			var blocks []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(e.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				switch {
				case b.Type == "text" && strings.TrimSpace(b.Text) != "":
					turns = append(turns, Turn{Role: "agent", Text: strings.TrimSpace(b.Text), At: e.Timestamp})
				case b.Type == "tool_use":
					turns = append(turns, Turn{Role: "tool", Text: ToolLine(b.Name, b.Input), At: e.Timestamp})
				}
			}
		}
	}
	return turns
}

// ToolLine: «Read: src/app.ts», «Bash: npm test» — the tool and what it was given, in a line.
func ToolLine(name string, input json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(input, &in)
	for _, key := range []string{"command", "file_path", "path", "pattern", "url", "query", "description", "prompt", "skill"} {
		if v, ok := in[key].(string); ok && strings.TrimSpace(v) != "" {
			v = strings.Join(strings.Fields(v), " ")
			if r := []rune(v); len(r) > 160 {
				v = string(r[:159]) + "…"
			}
			return name + ": " + v
		}
	}
	return name
}
