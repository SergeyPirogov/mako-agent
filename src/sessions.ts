import { readFileSync } from "node:fs";
import type { IPty } from "node-pty";

/**
 * working   – the agent is busy
 * waiting   – the agent needs the developer (a permission prompt, a question)
 * idle      – the agent finished its turn; review and press "Завершити" or give more instructions
 * exited    – the process ended; Mako is publishing (or published) the result
 */
export type SessionStatus = "starting" | "working" | "waiting" | "idle" | "exited";

export type SessionEvent =
  | { t: "o"; d: string }
  | { t: "s"; status: SessionStatus; message?: string }
  | { t: "x"; code: number | null };

const MAX_BUFFER = 2_000_000;

/** A live interactive agent in a pseudo-terminal that browsers can watch and type into. */
export class Session {
  status: SessionStatus = "starting";
  message?: string;
  /** When the agent started waiting for the developer (waiting or idle); undefined while it works. */
  attentionSince?: number;
  lastAssistantMessage?: string;
  /** Someone typed into the terminal, so the run is no longer hands-off. */
  userTouched = false;
  /** The developer typed since the agent started waiting: no more reminders for this wait. */
  acknowledged = false;
  ending?: "finish" | "cancel";
  exitCode: number | null = null;
  private buffer = "";
  private listeners = new Set<(e: SessionEvent) => void>();

  constructor(
    readonly id: string,
    readonly task: { id: string; title: string; url: string },
    readonly agentLabel: string,
    readonly hookToken: string,
    private pty: IPty,
    /** Text typed to leave the agent politely (e.g. "/exit" for Claude); otherwise the process is killed. */
    private exitCommand: string | undefined,
  ) {
    pty.onData((d) => {
      this.buffer = (this.buffer + d).slice(-MAX_BUFFER);
      this.emit({ t: "o", d });
    });
    pty.onExit(({ exitCode }) => {
      this.exitCode = exitCode;
      this.setStatus("exited");
      this.emit({ t: "x", code: exitCode });
    });
  }

  get output(): string {
    return this.buffer;
  }

  subscribe(fn: (e: SessionEvent) => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  input(data: string): void {
    if (this.status === "exited") return;
    this.userTouched = true;
    if (this.attentionSince) this.acknowledged = true;
    this.pty.write(data);
  }

  resize(cols: number, rows: number): void {
    if (this.status !== "exited" && cols > 10 && rows > 3) this.pty.resize(Math.floor(cols), Math.floor(rows));
  }

  /** Ends the session and lets Mako commit, push and open the PR. */
  /** Gives the agent a request as if the developer pasted it into the prompt and pressed Enter. */
  say(text: string): boolean {
    if (this.status === "exited") return false;
    this.pty.write(`\x1b[200~${text}\x1b[201~`);
    setTimeout(() => this.pty.write("\r"), 200);
    return true;
  }

  finish(): void {
    if (this.status === "exited") return;
    this.ending = "finish";
    if (this.exitCommand) {
      this.pty.write("\x1b"); // close any open menu or draft first
      setTimeout(() => this.pty.write(`${this.exitCommand}\r`), 300);
      setTimeout(() => this.kill(), 8000);
    } else {
      this.kill();
    }
  }

  cancel(): void {
    this.ending = "cancel";
    this.kill();
  }

  kill(): void {
    if (this.status === "exited") return;
    try {
      this.pty.kill();
    } catch {
      // already gone
    }
  }

  setStatus(status: SessionStatus, message?: string): void {
    if (this.status === "exited") return;
    if (status === this.status && message === this.message) return;
    const needsYou = status === "waiting" || status === "idle";
    if (needsYou && !(this.status === "waiting" || this.status === "idle")) this.attentionSince = Date.now();
    if (!needsYou) this.attentionSince = undefined;
    if (status !== this.status) this.acknowledged = false;
    this.status = status;
    this.message = message;
    this.emit({ t: "s", status, message });
  }

  /** Claude Code hook events (sent by the hooks Mako adds through --settings). */
  onHook(event: any): SessionStatus | undefined {
    switch (event?.hook_event_name) {
      case "UserPromptSubmit":
      case "PreToolUse":
      case "PostToolUse":
        this.setStatus("working");
        break;
      case "Notification":
        this.setStatus("waiting", humanize(typeof event.message === "string" ? event.message : ""));
        break;
      case "Stop":
        // Once the developer pressed "Завершити", the summary of the work is final.
        if (!this.ending) {
          this.lastAssistantMessage = event.last_assistant_message ?? lastAssistantText(event.transcript_path) ?? this.lastAssistantMessage;
        }
        this.setStatus("idle", "Можна перевіряти: подивіться результат і натисніть «Завершити» або дайте нові інструкції");
        break;
      default:
        return undefined;
    }
    return this.status;
  }

  private emit(e: SessionEvent): void {
    for (const fn of this.listeners) fn(e);
  }
}

export const sessions = new Map<string, Session>();

/** Sessions where the agent waits for the developer, longest-waiting first. */
export function attentionSessions(): Session[] {
  return [...sessions.values()]
    .filter((s) => s.status === "waiting" || s.status === "idle")
    .sort((a, b) => (a.attentionSince ?? 0) - (b.attentionSince ?? 0));
}

/** Claude Code's notification texts in Ukrainian; unknown texts pass through. */
export function humanize(message: string): string {
  const permission = message.match(/needs your permission to use (.+)$/i);
  if (permission) return `Просить дозволу: ${permission[1]}`;
  if (/waiting for your input/i.test(message)) return "Чекає на вашу відповідь";
  if (/needs your (approval|attention)/i.test(message)) return "Потрібне ваше рішення";
  return message || "Чекає на вас";
}

export function sessionIdFor(task: { trackerId: string; slug: string }): string {
  return `${task.trackerId}-${task.slug}`.replace(/[^\w.-]/g, "_");
}

/** Last assistant text from a Claude Code transcript (JSONL). */
function lastAssistantText(transcriptPath: unknown): string | undefined {
  if (typeof transcriptPath !== "string") return undefined;
  try {
    const lines = readFileSync(transcriptPath, "utf8").trim().split("\n");
    for (let i = lines.length - 1; i >= 0; i--) {
      const entry = JSON.parse(lines[i]);
      if (entry.type !== "assistant") continue;
      const content = entry.message?.content;
      const text = Array.isArray(content)
        ? content.filter((c: any) => c.type === "text").map((c: any) => c.text).join("\n")
        : typeof content === "string" ? content : "";
      if (text.trim()) return text.trim();
    }
  } catch {
    // transcript missing or in an unknown format
  }
  return undefined;
}

/** Plain text of the last part of a terminal recording, for summaries of agents without hooks. */
export function plainTail(output: string, max = 1500): string {
  return output
    .replace(/\x1b\][^\x07\x1b]*(\x07|\x1b\\)/g, "")
    .replace(/\x1b\[[0-9;?]*[ -/]*[@-~]/g, "")
    .replace(/\x1b[@-_]/g, "")
    .replace(/\r/g, "")
    .replace(/\n{3,}/g, "\n\n")
    .trim()
    .slice(-max);
}
