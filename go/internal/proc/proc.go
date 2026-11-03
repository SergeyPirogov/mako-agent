// Package proc starts the commands Mako runs so that ending them ends all they started: each runs in a process
// group of its own, which is killed as a whole, and Wait does not hang on a pipe a stray descendant still holds.
package proc

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// WaitDelay: how long Wait waits, once the command ended or its context did, for the pipes its descendants hold.
const WaitDelay = 5 * time.Second

// Command is exec.CommandContext in a process group of its own: the end of ctx kills the whole group, and its
// environment is Mako's without the marks of a Claude Code session (callers append to cmd.Env).
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return KillGroup(cmd.Process.Pid) }
	cmd.WaitDelay = WaitDelay
	cmd.Env = CleanEnv(os.Environ())
	return cmd
}

// KillGroup kills the process group led by pid.
func KillGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGKILL) }

// The marks of a Claude Code session Mako may have been started from: an agent that inherits them takes itself
// for that session's child and saves no transcript (so no last message and no --continue).
var sessionMarks = map[string]bool{
	"CLAUDECODE": true, "CLAUDE_PID": true, "CLAUDE_EFFORT": true, "CLAUDE_CODE_ENTRYPOINT": true,
	"CLAUDE_CODE_EXECPATH": true, "CLAUDE_CODE_SESSION_ID": true, "CLAUDE_CODE_SESSION_ATTENDED": true,
	"CLAUDE_CODE_CHILD_SESSION": true, "CLAUDE_CODE_MESSAGING_SOCKET": true, "CLAUDE_CODE_MESSAGING_TOKEN": true,
}

// CleanEnv drops the marks of the Claude Code session Mako may run in, so its agents are sessions of their own.
func CleanEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !sessionMarks[k] {
			out = append(out, kv)
		}
	}
	return out
}
