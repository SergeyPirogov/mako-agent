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

const order: TaskStatus[] = ["asking", "snoozed", "queued", "running", "failed", "handed-off", "done"];

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

/** Task list with the same Take / Postpone buttons as the notification, for ones that were missed. */
export function renderInbox(app: App, token: string): string {
  if (!app.orchestrator) {
    const body = `<h1>Вхідні</h1><div class="notice">${esc(app.problem ?? "Mako запускається…")}.
      <a href="/settings">Відкрити налаштування →</a></div>`;
    return page({ title: "Вхідні", active: "inbox", token, body });
  }
  const records = app.orchestrator.state.all();
  const rows = records
    .sort((a, b) => order.indexOf(a.status) - order.indexOf(b.status) || b.updatedAt.localeCompare(a.updatedAt))
    .map((r) => row(r, token))
    .join("");
  const problem = app.problem ? `<div class="notice err">${esc(app.problem)}. <a href="/settings">Налаштування →</a></div>` : "";
  const limit = app.cfg?.wipLimit ?? 0;
  const wip = app.orchestrator.wip();
  const wipBar = limit
    ? `<div class="wip${wip >= limit ? " full" : ""}"><span>В роботі: <b>${wip} / ${limit}</b></span>
        <span class="muted">${wip >= limit ? "Ліміт досягнуто: Mako не бере нових задач, поки одна не завершиться" : `Вільних місць: ${limit - wip}`}</span></div>`
    : "";
  const paused = app.orchestrator.paused
    ? `<div class="notice warn">⏸ Mako на паузі: нові задачі не беруться, поточні продовжують виконуватись.</div>`
    : "";
  // The clock is drawn and ticked by /ui/inbox.js from the search status.
  const clock = `<div id="clock" class="clock"><span id="clock-text" class="muted">…</span>
    <form method="post" action="/search-now" id="search-now" hidden><input type="hidden" name="token" value="${esc(token)}"><button>Перевірити зараз</button></form></div>`;
  const body = `<h1>Задачі для агента</h1>${problem}${paused}${attentionBlock()}${clock}${wipBar}
    ${rows ? `<table class="tasks">${rows}</table>` : `<p class="muted">Поки немає задач. Mako перевіряє трекери кожні ${app.cfg?.pollIntervalSec ?? 60} с.</p>`}`;
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

function row(r: TaskRecord, token: string): string {
  const form = (action: string, label: string, primary = false) =>
    `<form method="post" action="/${action}"><input type="hidden" name="id" value="${esc(r.id)}">` +
    `<input type="hidden" name="token" value="${esc(token)}"><button${primary ? ' class="primary"' : ""}>${label}</button></form>`;
  const session = r.status === "running" ? sessions.get(sessionIdFor(r.task)) : undefined;
  const actions = session
    ? `<a class="btn" href="/session/${esc(session.id)}">Термінал</a>`
    : r.status === "asking" || r.status === "snoozed"
      ? form("take", "Взяти", true) + form("later", "Відкласти")
      : r.status === "failed"
        ? form("retry", "Повторити")
        : "";
  const extra = session
    ? `<span class="s ${session.status}">${SESSION_LABELS[session.status]}</span>`
    : r.prUrl
    ? `<a href="${esc(r.prUrl)}">PR</a>`
    : r.error
      ? `<span class="err">${esc(r.error.slice(0, 160))}</span>`
      : r.status === "handed-off"
        ? `чекає PR з <code>${esc(r.branch ?? "")}</code>`
        : r.status === "snoozed" && r.snoozedUntil
          ? `до ${new Date(r.snoozedUntil).toLocaleTimeString()}`
          : "";
  const attn = r.status === "asking" || session?.status === "waiting" || session?.status === "idle";
  return `<tr${attn ? ' class="attn"' : ""}><td><span class="s ${r.status}">${LABELS[r.status]}</span></td>
    <td><a href="${esc(r.task.url)}">${esc(r.id)}</a>${r.repo ? ` → ${esc(r.repo)}` : ""}${r.executor ? ` · ${esc(r.executor)}` : ""}<div class="muted">${esc(r.task.title)}</div></td>
    <td>${extra}</td><td class="act">${actions}</td></tr>`;
}
