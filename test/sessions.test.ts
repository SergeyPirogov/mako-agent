import { test } from "node:test";
import assert from "node:assert/strict";
import type { IPty } from "node-pty";
import { Session, humanize, sessionIdFor } from "../src/sessions.ts";

// A terminal session as Mako sees it through Claude Code's hooks, with a stand-in for the pseudo-terminal.
function fakePty() {
  let onData: (d: string) => void = () => {};
  let onExit: (e: { exitCode: number }) => void = () => {};
  const written: string[] = [];
  const pty = {
    onData: (fn: typeof onData) => void (onData = fn),
    onExit: (fn: typeof onExit) => void (onExit = fn),
    write: (d: string) => void written.push(d),
    resize: () => {},
    kill: () => onExit({ exitCode: 0 }),
  } as unknown as IPty;
  return { pty, written, output: (d: string) => onData(d), exit: (code: number) => onExit({ exitCode: code }) };
}

const task = { id: "acme/web#1", title: "Fix it", url: "https://x" };

test("hooks drive the session state: working, waiting for the developer, finished a turn", () => {
  const p = fakePty();
  const s = new Session("gitlab-web-1", task, "Claude Code", "tok", p.pty, "/exit");
  const seen: string[] = [];
  s.subscribe((e) => e.t === "s" && seen.push(`${e.status}${e.message ? `: ${e.message}` : ""}`));

  s.onHook({ hook_event_name: "UserPromptSubmit" });
  s.onHook({ hook_event_name: "Notification", message: "Claude needs your permission to use Bash" });
  assert.equal(s.status, "waiting");
  assert.ok(s.attentionSince, "the board can say how long it has been waiting");
  s.input("y");
  assert.equal(s.acknowledged, true, "typing in the terminal stops the reminders");
  s.onHook({ hook_event_name: "PostToolUse" });
  s.onHook({ hook_event_name: "Stop", last_assistant_message: "Added the test." });
  assert.equal(s.status, "idle");
  assert.equal(s.lastAssistantMessage, "Added the test.");
  assert.deepEqual(seen, [
    "working",
    "waiting: Просить дозволу: Bash",
    "working",
    "idle: Можна перевіряти: подивіться результат і натисніть «Завершити» або дайте нові інструкції",
  ]);
  assert.equal(s.onHook({ hook_event_name: "SomethingNew" }), undefined, "unknown hooks are ignored");
});

test("finishing keeps the summary and leaves the agent politely", () => {
  const p = fakePty();
  const s = new Session("id", task, "Claude Code", "tok", p.pty, "/exit");
  s.onHook({ hook_event_name: "Stop", last_assistant_message: "Done: tests pass." });
  s.finish();
  s.onHook({ hook_event_name: "Stop", last_assistant_message: "Bye!" });
  assert.equal(s.lastAssistantMessage, "Done: tests pass.", "the reply to /exit is not the summary");
  p.exit(0);
  assert.equal(s.status, "exited");
  assert.equal(s.say("more"), false, "nothing reaches an agent that has exited");
});

test("output is kept for pages opened later; ids and messages are readable", () => {
  const p = fakePty();
  const s = new Session("id", task, "Claude Code", "tok", p.pty, undefined);
  p.output("hello ");
  p.output("world");
  assert.equal(s.output, "hello world");
  assert.equal(sessionIdFor({ trackerId: "gitlab", slug: "web-1" }), "gitlab-web-1");
  assert.equal(humanize("Claude is waiting for your input"), "Чекає на вашу відповідь");
  assert.equal(humanize(""), "Чекає на вас");
});
