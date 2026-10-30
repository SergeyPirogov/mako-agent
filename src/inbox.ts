import type { App } from "./app.js";
import type { TaskRecord, TaskStatus } from "./state.js";
import { esc, page } from "./ui-layout.js";
import { sessions, sessionIdFor, attentionSessions } from "./sessions.js";
import { recentLog } from "./log.js";

const LABELS: Record<TaskStatus, string> = {
  asking: "Нова",
  snoozed: "Відкладена",
  queued: "У черзі",
  running: "Виконується",
  "handed-off": "Передано",
  done: "Готово",
  failed: "Помилка",
  cancelled: "Скасовано",
};

const SESSION_LABELS = { starting: "запуск", working: "працює", waiting: "чекає на вас", idle: "закінчив хід", exited: "завершено" };

/** Changes whenever the inbox would look different; the page polls it to refresh itself. */
export function inboxSignature(app: App): { sig: string; attention: number; search?: unknown } {
  const records = app.orchestrator?.state.all() ?? [];
  const live = [...sessions.values()].map((s) => `${s.id}:${s.status}:${s.message ?? ""}`);
  const asking = records.filter((r) => r.status === "asking" || (r.status === "failed" && r.denied?.length && !r.archivedAt)).length;
  return {
    sig: JSON.stringify([app.problem, app.orchestrator?.paused, records.map((r) => `${r.id}:${r.status}:${r.updatedAt}:${r.status === "running" ? recentLog(r.id).length : ""}`), live]),
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
  { key: "failed", title: "Не виконано", statuses: ["failed", "cancelled"], empty: "" },
];

const DONE_SHOWN = 15;

/** Kanban board: Нові → В роботі (WIP limit) → Готово, plus Помилка when something failed. */
export function renderInbox(app: App, token: string): string {
  if (!app.orchestrator) {
    const body = `<h1>Задачі</h1><div class="notice">${esc(app.problem ?? "Mako запускається…")}.
      <a href="/settings">Відкрити налаштування →</a></div>`;
    return page({ title: "Задачі", active: "inbox", token, body });
  }
  const all = app.orchestrator.state.all();
  const records = all.filter((r) => !r.archivedAt);
  const archived = all.length - records.length;
  const problem = app.problem ? `<div class="notice err">${esc(app.problem)}. <a href="/settings">Налаштування →</a></div>` : "";
  const paused = app.orchestrator.paused
    ? `<div class="notice warn">⏸ Mako на паузі: нові задачі не беруться, поточні продовжують виконуватись.</div>`
    : "";
  // The clock is drawn and ticked by /ui/inbox.js from the search status.
  const clock = `<div id="clock" class="clock"><span id="clock-text" class="muted">…</span>
    <form method="post" action="/search-now" id="search-now" hidden><input type="hidden" name="token" value="${esc(token)}"><button>Перевірити зараз</button></form></div>`;

  const limit = app.cfg?.wipLimit ?? 0;
  const wip = app.orchestrator.wip();
  const columns = COLUMNS.filter((c) => c.key !== "failed" || records.some((r) => r.status === "failed" || r.status === "cancelled"));
  const board = columns
    .map((col) => {
      let items = records.filter((r) => col.statuses.includes(r.status)).sort(columnOrder(col.key));
      const total = items.length;
      if (col.key === "done") items = items.slice(0, DONE_SHOWN);
      const archiveAll =
        (col.key === "done" || col.key === "failed") && total
          ? `<form method="post" action="/archive" class="karchive"><input type="hidden" name="column" value="${col.key}">` +
            `<input type="hidden" name="token" value="${esc(token)}"><button class="link" title="Перенести всі задачі колонки в архів">Архівувати всі</button></form>`
          : "";
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
      return `<section class="kcol" data-col="${col.key}"><header><h2>${col.title}</h2><span class="khead-tools">${archiveAll}${count}</span></header>${note}
        <div class="kcards">${cards || `<div class="kempty">${col.empty}</div>`}${more}</div></section>`;
    })
    .join("");

  const archiveLink = archived ? `<a class="link archive-link" href="/archive">Архів (${archived})</a>` : "";
  const body = `<div class="board-head"><h1>Задачі</h1>${archiveLink}${clock}</div>${problem}${paused}${attentionBlock()}
    <div class="board" style="--cols:${columns.length}">${board}</div>`;
  const { sig, attention, search } = inboxSignature(app);
  return page({
    title: `${attention ? `(${attention}) ` : ""}Задачі`,
    active: "inbox",
    token,
    body,
    script: "/ui/inbox.js",
    data: { sig, search: JSON.stringify(search ?? null) },
    attention: attention > 0,
  });
}

/** Archived tasks, newest first, with a way back to the board. */
export function renderArchive(app: App, token: string): string {
  const records = (app.orchestrator?.state.all() ?? []).filter((r) => r.archivedAt).sort((a, b) => (b.archivedAt ?? 0) - (a.archivedAt ?? 0));
  const day = (ms: number) => new Date(ms).toLocaleString("uk-UA", { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" });
  const rows = records
    .map((r) => {
      const actions = [
        app.orchestrator?.workspaceFor(r.id) ? `<a class="link" href="/changes?task=${encodeURIComponent(r.id)}">Зміни</a>` : "",
        r.prUrl ? `<a class="link" href="${esc(r.prUrl)}" target="_blank" rel="noreferrer">PR ↗</a>` : "",
        `<form method="post" action="/unarchive"><input type="hidden" name="id" value="${esc(r.id)}"><input type="hidden" name="token" value="${esc(token)}">` +
          `<input type="hidden" name="back" value="/archive"><button>Повернути на дошку</button></form>`,
      ].join("");
      return `<tr><td><span class="s ${r.status}">${LABELS[r.status]}</span></td>
        <td><a href="${esc(r.task.url)}" target="_blank" rel="noreferrer">${esc(r.id)}</a> ${route(app, r)}<div class="muted">${esc(r.task.title)}</div>
          ${r.error ? `<div class="kc-error">${errorDetails(r.error)}</div>` : ""}</td>
        <td class="muted small">${day(r.archivedAt!)}</td><td class="act">${actions}</td></tr>`;
    })
    .join("");
  const body = `<div class="board-head"><h1>Архів</h1><a class="link" href="/">← До дошки</a></div>
    ${rows ? `<table class="tasks archive">${rows}</table>` : `<p class="muted">Архів порожній. На дошці кнопка «В архів» прибирає виконані й невиконані задачі сюди.</p>`}`;
  return page({ title: "Архів", active: "inbox", token, body });
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

/** Where the task runs (or would run if taken now): repo · executor. */
function route(app: App, r: TaskRecord): string {
  const waiting = r.status === "asking" || r.status === "snoozed";
  const choices = app.orchestrator?.choicesFor(r.id);
  const repo = waiting ? (choices?.repo ?? r.repo) : r.repo;
  const name = waiting ? choices?.executor : r.executor;
  const executor = choices?.executors.find((e) => e.name === name)?.label ?? name;
  if (!repo && !executor) return "";
  return `<span class="kc-repo" title="Куди піде задача">${esc([repo, executor].filter(Boolean).join(" · "))}</span>`;
}

/** "Взяти" runs the task the usual way; "▾" lets the developer pick the executor and, for multi-repo projects, the repo. */
function takeControl(app: App, r: TaskRecord, token: string, action: "take" | "retry"): string {
  const hidden = `<input type="hidden" name="id" value="${esc(r.id)}"><input type="hidden" name="token" value="${esc(token)}">`;
  const verb = action === "take" ? "Взяти" : r.status === "done" ? "Ще раз" : "Повторити";
  const quick = `<form method="post" action="/${action}">${hidden}<button class="${action === "take" ? "primary" : "soft"} take-main">${verb}</button></form>`;
  const choices = app.orchestrator?.choicesFor(r.id);
  if (!choices || (choices.executors.length < 2 && choices.repos.length < 2)) return quick;
  const option = (value: string, label: string, selected: boolean) =>
    `<option value="${esc(value)}"${selected ? " selected" : ""}>${esc(label)}</option>`;
  const executors = choices.executors
    .map((e) => option(e.name, `${e.label}${e.name === choices.executor ? " (за замовчуванням)" : ""}`, e.name === choices.executor))
    .join("");
  const repos =
    choices.repos.length > 1
      ? `<label>Репозиторій<select name="repo">${choices.repos.map((n) => option(n, n, n === choices.repo)).join("")}</select></label>`
      : "";
  return `<div class="take ${action}">${quick}<details class="take-menu"><summary class="take-more" title="Вибрати, куди відправити задачу">▾</summary>
    <form method="post" action="/${action}" class="take-pop">${hidden}
      <div class="tp-title">Куди відправити</div>
      <label>Хто виконує<select name="executor">${executors}</select></label>
      ${repos}
      <button class="primary">${verb}</button>
    </form></details></div>`;
}

function card(app: App, r: TaskRecord, token: string, col: Column["key"]): string {
  const form = (action: string, label: string, primary = false) =>
    `<form method="post" action="/${action}"><input type="hidden" name="id" value="${esc(r.id)}">` +
    `<input type="hidden" name="token" value="${esc(token)}"><button${primary ? ' class="primary"' : ""}>${label}</button></form>`;
  const session = r.status === "running" ? sessions.get(sessionIdFor(r.task)) : undefined;
  const actions = [
    session ? `<a class="btn" href="/session/${esc(session.id)}">Термінал</a>` : "",
    // No terminal yet (the worktree is being prepared) or none at all (a background agent): show its log.
    r.status === "running" && !session ? `<a class="btn" href="/log?task=${encodeURIComponent(r.id)}">Хід роботи</a>` : "",
    r.status === "asking" || r.status === "snoozed" ? takeControl(app, r, token, "take") + form("later", "Відкласти") : "",
    r.gone !== "deleted" && ["done", "failed", "cancelled"].includes(r.status) && r.sessionId && r.executor !== app.orchestrator?.terminalExecutor() && app.orchestrator?.terminalExecutor()
      ? `<form method="post" action="/retry" title="Відкрити ту саму розмову з агентом у терміналі Mako"><input type="hidden" name="id" value="${esc(r.id)}"><input type="hidden" name="token" value="${esc(token)}"><input type="hidden" name="resume" value="1"><button class="${r.status === "done" ? "soft" : "primary"}">Продовжити в терміналі</button></form>`
      : "",
    r.gone !== "deleted" && ["done", "failed", "cancelled"].includes(r.status) ? takeControl(app, r, token, "retry") : "",
    r.status === "queued" || r.status === "handed-off"
      ? `<form method="post" action="/cancel" data-confirm="${esc(
          r.status === "handed-off"
            ? "Mako перестане чекати PR і звільнить місце в роботі. Сам workspace в Orca чи іншому інструменті не зачіпається."
            : "Задача не запуститься і звільнить місце в роботі.",
        )}"><input type="hidden" name="id" value="${esc(r.id)}"><input type="hidden" name="token" value="${esc(token)}"><button class="danger-outline">Скасувати</button></form>`
      : "",
    app.orchestrator?.workspaceFor(r.id) ? `<a class="link" href="/changes?task=${encodeURIComponent(r.id)}">Зміни</a>` : "",
    r.prUrl ? `<a class="link" href="${esc(r.prUrl)}" target="_blank" rel="noreferrer">PR ↗</a>` : "",
    ["done", "failed", "cancelled"].includes(r.status)
      ? `<form method="post" action="/archive" class="kc-archive"><input type="hidden" name="id" value="${esc(r.id)}"><input type="hidden" name="token" value="${esc(token)}"><button class="link muted-link" title="Прибрати з дошки в архів">В архів</button></form>`
      : "",
  ].join("");
  const status = session
    ? `<span class="s ${session.status}">${SESSION_LABELS[session.status]}</span>${session.message && session.status === "waiting" ? ` <span class="muted">${esc(session.message)}</span>` : ""}`
    : `<span class="s ${r.status}">${LABELS[r.status]}</span>${
        r.status === "handed-off" ? ` <span class="muted">чекає PR з <code>${esc(r.branch ?? "")}</code></span>`
        : r.status === "running" && recentLog(r.id).length ? ` <span class="muted kc-phase">${esc(phase(recentLog(r.id).at(-1)!.text))}</span>`
        : r.status === "snoozed" && r.snoozedUntil ? ` <span class="muted">до ${new Date(r.snoozedUntil).toLocaleTimeString("uk-UA", { hour: "2-digit", minute: "2-digit" })}</span>`
        : ""
      }`;
  const needsPermission = r.status === "failed" && Boolean(r.denied?.length);
  const trackerName = app.orchestrator?.trackers.get(r.task.trackerId)?.name ?? "трекері";
  const gone = r.gone
    ? `<span class="s gone ${r.gone}" title="${r.gone === "deleted" ? "Задачі більше немає в трекері" : "Задачу закрили в трекері"}">${r.gone === "deleted" ? `🗑 Видалена в ${esc(trackerName)}` : `✓ Закрита в ${esc(trackerName)}`}</span>`
    : "";
  const attn = r.status === "asking" || session?.status === "waiting" || session?.status === "idle" || needsPermission;
  const draggable = col === "new" || col === "failed";
  return `<article class="kcard${attn ? " attn" : ""}"${draggable ? ` draggable="true" data-id="${esc(r.id)}" data-from="${col}"` : ""}>
    <div class="kc-top"><a class="kc-id" href="${esc(r.task.url)}" target="_blank" rel="noreferrer">${esc(r.id)}</a>
      ${route(app, r)}</div>
    <div class="kc-title">${esc(r.task.title)}</div>
    ${description(r)}
    <div class="kc-status">${gone ? (r.status === "cancelled" ? gone : `${status} ${gone}`) : needsPermission ? `<span class="s perm">🔒 Потрібні дозволи</span>` : status}</div>
    ${needsPermission
      ? `<div class="kc-perm"><div class="small muted">Агенту не дозволили:</div><ul>${r.denied!.slice(0, 3).map((d) => `<li><code>${esc(d)}</code></li>`).join("")}${r.denied!.length > 3 ? `<li class="muted">і ще ${r.denied!.length - 3}</li>` : ""}</ul></div>`
      : r.error && !(r.gone && r.status === "cancelled") ? `<div class="kc-error">${errorDetails(r.error)}</div>` : ""}
    ${actions ? `<div class="kc-actions">${actions}</div>` : ""}
  </article>`;
}

/** The latest step of a running task in a few words. */
function phase(line: string): string {
  const t = line.replace(/\s+/g, " ").trim();
  return t.length > 90 ? t.slice(0, 89) + "…" : t;
}

/** What a task without a terminal is doing: Mako's own steps and, for background agents, their actions. */
export function renderTaskLog(app: App, id: string, token: string): string {
  const r = app.orchestrator?.state.get(id);
  const lines = recentLog(id);
  const time = (at: number) => new Date(at).toLocaleTimeString("uk-UA", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  const live = r?.status === "running" || r?.status === "queued";
  const body = `<div class="board-head"><h1>Хід роботи</h1><a class="link" href="/">← До дошки</a></div>
    ${r ? `<p><a href="${esc(r.task.url)}" target="_blank" rel="noreferrer">${esc(r.id)}</a> ${esc(r.task.title)} · <span class="s ${r.status}">${LABELS[r.status]}</span></p>` : ""}
    ${lines.length
      ? `<ol class="tlog">${[...lines].reverse().map((l) => `<li><span class="muted">${time(l.at)}</span> ${esc(l.text)}</li>`).join("")}</ol>`
      : `<p class="muted">Поки що записів немає${live ? "" : " (журнал зберігається, доки Mako запущений)"}.</p>`}
    ${live ? `<p class="small muted">Нові записи зверху, сторінка оновлюється сама.</p>` : ""}`;
  return page({ title: `Хід роботи ${id}`, active: "inbox", token, body, refresh: live ? 3 : undefined });
}

/** A short plain-text preview on the card; the whole description opens in a dialog. */
function description(r: TaskRecord): string {
  const full = (r.task.description ?? "").trim();
  if (!full) return "";
  const short = plainText(full);
  if (!short) return "";
  const cut = short.length > 160 ? short.slice(0, 159).replace(/\s+\S*$/, "") + "…" : short;
  return `<button type="button" class="kc-desc" data-desc title="Показати повний опис">${esc(cut)}</button>
    <template class="kc-full"><h2>${esc(r.task.title)}</h2>
      <p class="muted small"><a href="${esc(r.task.url)}" target="_blank" rel="noreferrer">${esc(r.id)} ↗</a></p>
      <div class="desc-body">${richText(full)}</div></template>`;
}

/** Markdown without its markup, for a one-paragraph preview. */
function plainText(md: string): string {
  return md
    .replace(/<!--[\s\S]*?-->/g, " ")
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/!\[[^\]]*\]\([^)]*\)/g, " ")
    .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/<[^>]+>/g, " ")
    .replace(/^\s{0,3}(#{1,6}|[-*+>]|\d+\.)\s+/gm, "")
    .replace(/[*_`~]+/g, "")
    .replace(/\s+/g, " ")
    .trim();
}

/** The description as safe HTML: escaped, with code blocks, headings, lists, bold, inline code and links. */
function richText(md: string): string {
  const inline = (t: string) =>
    esc(t)
      .replace(/`([^`]+)`/g, "<code>$1</code>")
      .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
      .replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noreferrer">$1</a>')
      .replace(/(^|[\s(])(https?:\/\/[^\s<)]+)/g, '$1<a href="$2" target="_blank" rel="noreferrer">$2</a>');
  const out: string[] = [];
  const blocks = md.replace(/<!--[\s\S]*?-->/g, "").split(/```/);
  blocks.forEach((block, i) => {
    if (i % 2) return out.push(`<pre>${esc(block.replace(/^[\w-]*\n/, ""))}</pre>`);
    let list: string[] = [];
    const flush = () => {
      if (list.length) out.push(`<ul>${list.map((li) => `<li>${inline(li)}</li>`).join("")}</ul>`);
      list = [];
    };
    for (const para of block.split(/\n{2,}/)) {
      for (const line of para.split("\n")) {
        const item = line.match(/^\s*(?:[-*+]|\d+\.)\s+(?:\[[ x]\]\s+)?(.*)$/);
        if (item) {
          list.push(item[1]);
          continue;
        }
        flush();
        const head = line.match(/^\s{0,3}#{1,6}\s+(.*)$/);
        if (head) out.push(`<h3>${inline(head[1])}</h3>`);
        else if (line.trim()) out.push(`<p>${inline(line.replace(/^\s*>\s?/, ""))}</p>`);
      }
      flush();
    }
  });
  return out.join("");
}
