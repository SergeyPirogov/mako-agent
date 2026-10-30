import type { App } from "./app.js";
import type { TaskRecord, TaskStatus } from "./state.js";
import { esc, page } from "./ui-layout.js";
import { sessions, sessionIdFor, attentionSessions } from "./sessions.js";

const LABELS: Record<TaskStatus, string> = {
  asking: "Нова",
  snoozed: "Відкладена",
  queued: "У черзі",
  running: "Виконується",
  "handed-off": "Передано",
  done: "Готово",
  failed: "Помилка",
};

const SESSION_LABELS = { starting: "запуск", working: "працює", waiting: "чекає на вас", idle: "закінчив хід", exited: "завершено" };

/** Changes whenever the inbox would look different; the page polls it to refresh itself. */
export function inboxSignature(app: App): { sig: string; attention: number; search?: unknown } {
  const records = app.orchestrator?.state.all() ?? [];
  const live = [...sessions.values()].map((s) => `${s.id}:${s.status}:${s.message ?? ""}`);
  const asking = records.filter((r) => r.status === "asking").length;
  return {
    sig: JSON.stringify([app.problem, app.orchestrator?.paused, records.map((r) => `${r.id}:${r.status}:${r.updatedAt}`), live]),
    attention: attentionSessions().length + asking,
    search: app.orchestrator?.searchStatus(),
  };
}

interface Column {
  key: "new" | "doing" | "done" | "failed";
  title: string;
  statuses: TaskStatus[];
  empty: string;
}

const COLUMNS: Column[] = [
  { key: "new", title: "Нові", statuses: ["asking", "snoozed"], empty: "Нових задач немає" },
  { key: "doing", title: "В роботі", statuses: ["queued", "running", "handed-off"], empty: "Перетягніть сюди задачу з «Нових», щоб взяти її" },
  { key: "done", title: "Готово", statuses: ["done"], empty: "Тут зʼявляться задачі з PR" },
  { key: "failed", title: "Помилка", statuses: ["failed"], empty: "" },
];

const DONE_SHOWN = 15;

/** Kanban board: Нові → В роботі (WIP limit) → Готово, plus Помилка when something failed. */
export function renderInbox(app: App, token: string): string {
  if (!app.orchestrator) {
    const body = `<h1>Вхідні</h1><div class="notice">${esc(app.problem ?? "Mako запускається…")}.
      <a href="/settings">Відкрити налаштування →</a></div>`;
    return page({ title: "Вхідні", active: "inbox", token, body });
  }
  const records = app.orchestrator.state.all();
  const problem = app.problem ? `<div class="notice err">${esc(app.problem)}. <a href="/settings">Налаштування →</a></div>` : "";
  const paused = app.orchestrator.paused
    ? `<div class="notice warn">⏸ Mako на паузі: нові задачі не беруться, поточні продовжують виконуватись.</div>`
    : "";
  // The clock is drawn and ticked by /ui/inbox.js from the search status.
  const clock = `<div id="clock" class="clock"><span id="clock-text" class="muted">…</span>
    <form method="post" action="/search-now" id="search-now" hidden><input type="hidden" name="token" value="${esc(token)}"><button>Перевірити зараз</button></form></div>`;

  const limit = app.cfg?.wipLimit ?? 0;
  const wip = app.orchestrator.wip();
  const columns = COLUMNS.filter((c) => c.key !== "failed" || records.some((r) => r.status === "failed"));
  const board = columns
    .map((col) => {
      let items = records.filter((r) => col.statuses.includes(r.status)).sort(columnOrder(col.key));
      const total = items.length;
      if (col.key === "done") items = items.slice(0, DONE_SHOWN);
      const count =
        col.key === "doing" && limit
          ? `<span class="kcount${wip >= limit ? " full" : ""}" title="Ліміт задач у роботі (WIP)">${wip} / ${limit}</span>`
          : `<span class="kcount">${total}</span>`;
      const note =
        col.key === "doing" && limit && wip >= limit
          ? `<div class="knote">Ліміт досягнуто: нові задачі не беруться, поки одна не завершиться</div>`
          : "";
      const cards = items.map((r) => card(app, r, token, col.key)).join("");
      const more = total > items.length ? `<div class="kmore muted">і ще ${total - items.length}</div>` : "";
      return `<section class="kcol" data-col="${col.key}"><header><h2>${col.title}</h2>${count}</header>${note}
        <div class="kcards">${cards || `<div class="kempty">${col.empty}</div>`}${more}</div></section>`;
    })
    .join("");

  const body = `<div class="board-head"><h1>Задачі</h1>${clock}</div>${problem}${paused}${attentionBlock()}
    <div class="board" style="--cols:${columns.length}">${board}</div>`;
  const { sig, attention, search } = inboxSignature(app);
  return page({
    title: `${attention ? `(${attention}) ` : ""}Вхідні`,
    active: "inbox",
    token,
    body,
    script: "/ui/inbox.js",
    data: { sig, search: JSON.stringify(search ?? null) },
    attention: attention > 0,
  });
}

/** Within a column: what needs you first, then the most recent. */
function columnOrder(col: Column["key"]) {
  const time = (r: TaskRecord) => r.finishedAt ?? r.updatedAt;
  const needsYou = (r: TaskRecord) => {
    const s = r.status === "running" ? sessions.get(sessionIdFor(r.task)) : undefined;
    return r.status === "asking" || s?.status === "waiting" || s?.status === "idle" ? 0 : 1;
  };
  return (a: TaskRecord, b: TaskRecord) => {
    if (col === "new" || col === "doing") {
      const d = needsYou(a) - needsYou(b);
      if (d) return d;
      if (col === "doing") return (a.queuedAt ?? 0) - (b.queuedAt ?? 0);
    }
    return time(b).localeCompare(time(a));
  };
}

/** A failure's first line, with the full text (cause, next steps) one click away. */
function errorDetails(error: string): string {
  const first = error.split("\n")[0];
  const short = first.length > 140 ? first.slice(0, 139) + "…" : first;
  if (short === error) return `<span class="err">${esc(error)}</span>`;
  return `<details class="errd"><summary class="err">${esc(short)}</summary><pre>${esc(error)}</pre></details>`;
}

/** What agents want from the developer right now, longest-waiting first. */
function attentionBlock(): string {
  const waiting = attentionSessions();
  if (!waiting.length) return "";
  const items = waiting
    .map((s) => {
      const minutes = s.attentionSince ? Math.floor((Date.now() - s.attentionSince) / 60_000) : 0;
      const since = minutes < 1 ? "щойно" : `${minutes} хв тому`;
      const what = s.status === "waiting" ? "чекає на вас" : "закінчив, перевірте";
      return `<a class="att ${s.status}" href="/session/${esc(s.id)}">
        <span class="att-dot"></span>
        <span class="att-text"><strong>${esc(s.task.id)}</strong> ${esc(s.task.title)}
          <span class="muted">${esc(s.agentLabel)} ${what} · ${esc(s.message ?? "")} · ${since}</span></span>
        <span class="btn">Відкрити</span></a>`;
    })
    .join("");
  return `<section class="attention"><h2>🔔 Потрібна ваша увага</h2>${items}</section>`;
}

function card(app: App, r: TaskRecord, token: string, col: Column["key"]): string {
  const form = (action: string, label: string, primary = false) =>
    `<form method="post" action="/${action}"><input type="hidden" name="id" value="${esc(r.id)}">` +
    `<input type="hidden" name="token" value="${esc(token)}"><button${primary ? ' class="primary"' : ""}>${label}</button></form>`;
  const session = r.status === "running" ? sessions.get(sessionIdFor(r.task)) : undefined;
  const actions = [
    session ? `<a class="btn" href="/session/${esc(session.id)}">Термінал</a>` : "",
    r.status === "asking" || r.status === "snoozed" ? form("take", "Взяти", true) + form("later", "Відкласти") : "",
    r.status === "failed" ? form("retry", "Повторити") : "",
    app.orchestrator?.workspaceFor(r.id) ? `<a class="link" href="/changes?task=${encodeURIComponent(r.id)}">Зміни</a>` : "",
    r.prUrl ? `<a class="link" href="${esc(r.prUrl)}" target="_blank" rel="noreferrer">PR ↗</a>` : "",
  ].join("");
  const status = session
    ? `<span class="s ${session.status}">${SESSION_LABELS[session.status]}</span>${session.message && session.status === "waiting" ? ` <span class="muted">${esc(session.message)}</span>` : ""}`
    : `<span class="s ${r.status}">${LABELS[r.status]}</span>${
        r.status === "handed-off" ? ` <span class="muted">чекає PR з <code>${esc(r.branch ?? "")}</code></span>`
        : r.status === "snoozed" && r.snoozedUntil ? ` <span class="muted">до ${new Date(r.snoozedUntil).toLocaleTimeString("uk-UA", { hour: "2-digit", minute: "2-digit" })}</span>`
        : ""
      }`;
  const attn = r.status === "asking" || session?.status === "waiting" || session?.status === "idle";
  const draggable = col === "new" || col === "failed";
  return `<article class="kcard${attn ? " attn" : ""}"${draggable ? ` draggable="true" data-id="${esc(r.id)}" data-from="${col}"` : ""}>
    <div class="kc-top"><a class="kc-id" href="${esc(r.task.url)}" target="_blank" rel="noreferrer">${esc(r.id)}</a>
      ${r.repo ? `<span class="kc-repo">${esc(r.repo)}${r.executor ? ` · ${esc(r.executor)}` : ""}</span>` : ""}</div>
    <div class="kc-title">${esc(r.task.title)}</div>
    <div class="kc-status">${status}</div>
    ${r.error ? `<div class="kc-error">${errorDetails(r.error)}</div>` : ""}
    ${actions ? `<div class="kc-actions">${actions}</div>` : ""}
  </article>`;
}
