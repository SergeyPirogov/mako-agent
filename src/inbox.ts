import type { App } from "./app.js";
import type { TaskRecord, TaskStatus } from "./state.js";
import { esc, page } from "./ui-layout.js";
import { sessions, sessionIdFor, attentionSessions } from "./sessions.js";
import { recentLog } from "./log.js";
import { renderMarkdown } from "./markdown.js";

const LABELS: Record<TaskStatus, string> = {
  asking: "Нова",
  snoozed: "Відкладена",
  queued: "У черзі",
  planned: "План готовий",
  running: "Виконується",
  "handed-off": "Передано",
  done: "Готово",
  failed: "Помилка",
  cancelled: "Скасовано",
};

const SESSION_LABELS = { starting: "запуск", working: "працює", waiting: "чекає на вас", idle: "можна перевіряти", exited: "завершено" };

/** Changes whenever the inbox would look different; the page polls it to refresh itself. */
export function inboxSignature(app: App): { sig: string; attention: number; search?: unknown; phases: Record<string, string> } {
  const records = app.orchestrator?.state.all() ?? [];
  const live = [...sessions.values()].map((s) => `${s.id}:${s.status}:${s.message ?? ""}`);
  const asking = records.filter((r) => r.status === "asking" || (r.status === "failed" && r.denied?.length && !r.archivedAt)).length;
  return {
    sig: JSON.stringify([app.problem, app.orchestrator?.paused, records.map((r) => `${r.id}:${r.status}:${r.updatedAt}`), live]),
    attention: attentionSessions().length + asking,
    // The latest step of background runs, updated in place: reloading the board for each log line made it jump.
    phases: Object.fromEntries(
      records.filter((r) => r.status === "running").map((r) => [r.id, phase(recentLog(r.id).at(-1)?.text ?? "")]),
    ),
    search: app.orchestrator?.searchStatus(),
  };
}

interface Column {
  key: "new" | "doing" | "review" | "done" | "failed";
  title: string;
  empty: string;
}

const COLUMNS: Column[] = [
  { key: "new", title: "Нові", empty: "Нових задач немає" },
  { key: "doing", title: "В роботі", empty: "Перетягніть сюди задачу з «Нових», щоб взяти її" },
  { key: "review", title: "На ревʼю", empty: "Тут задачі з відкритим MR, поки його не змерджать" },
  { key: "done", title: "Готово", empty: "Тут змерджені й завершені задачі" },
  { key: "failed", title: "Не виконано", empty: "" },
];

/** Which column a task is in: a finished task with an open PR/MR is not done until it is merged. */
export function columnOf(r: TaskRecord): Column["key"] {
  if (r.status === "asking" || r.status === "snoozed") return "new";
  // An open MR is in review, also while its terminal session is still open.
  if (r.status === "running" && r.prUrl && !r.reviewState) return "review";
  if (r.status === "queued" || r.status === "planned" || r.status === "running" || r.status === "handed-off") return "doing";
  if (r.status === "failed" || r.status === "cancelled" || r.reviewState === "closed") return "failed";
  return r.prUrl && !r.reviewState ? "review" : "done";
}

const DONE_SHOWN = 15;

/** Kanban board: Нові → В роботі (WIP limit) → Готово, plus Помилка when something failed. */
export function renderInbox(app: App, token: string, notice?: string): string {
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
  const columns = COLUMNS.filter((c) => c.key !== "failed" || records.some((r) => columnOf(r) === "failed"));
  const board = columns
    .map((col) => {
      let items = records.filter((r) => columnOf(r) === col.key).sort(columnOrder(col.key));
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
  const newTask =
    `<button type="button" class="soft new-task-btn" id="new-task-open">+ Задача</button>${newTaskDialog(app, token)}` +
    `<input type="search" id="board-filter" class="board-filter" placeholder="Пошук задач  /" title="Номер, назва, репозиторій, виконавець, мітка. Клавіша / — швидкий пошук" aria-label="Пошук задач на дошці" autocomplete="off">`;
  const explain = `<button type="button" class="link explain-open" id="explain-open" title="Перевірити, чому задачі з трекера немає на дошці">Не бачите задачу?</button>
    <dialog id="explain" class="mdialog wide-form"><form id="explain-form">
      <h2>Чому Mako не бачить задачу?</h2>
      <label class="f">Посилання на задачу або її номер<input type="text" name="ref" required placeholder="https://gitlab…/group/project/-/issues/12 або group/project#12" autocomplete="off"></label>
      <div id="explain-result" class="explain-result" aria-live="polite"></div>
      <div class="dlg-actions"><button type="button" data-close>Закрити</button><button class="primary">Перевірити</button></div>
    </form></dialog>`;
  const body = `<div class="board-head"><h1>Задачі</h1>${newTask}${explain}${archiveLink}${clock}</div>${problem}${paused}${notice ? `<div class="notice warn">${esc(notice)}</div>` : ""}${attentionBlock()}
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
        r.prUrl ? `<a class="link" href="${esc(r.prUrl)}" target="_blank" rel="noreferrer">${reviewKind(app, r)} ↗</a>` : "",
        `<form method="post" action="/unarchive"><input type="hidden" name="id" value="${esc(r.id)}"><input type="hidden" name="token" value="${esc(token)}">` +
          `<input type="hidden" name="back" value="/archive"><button>Повернути на дошку</button></form>`,
        `<form method="post" action="/delete" data-busy="Видаляю…" data-confirm-title="Видалити задачу?" data-confirm-ok="Видалити" data-confirm="${esc(DELETE_NOTE)}">` +
          `<input type="hidden" name="id" value="${esc(r.id)}"><input type="hidden" name="token" value="${esc(token)}"><button class="danger-outline">Видалити</button></form>`,
      ].join("");
      return `<tr><td><span class="s ${r.status}">${LABELS[r.status]}</span></td>
        <td><a href="${esc(r.task.url)}" target="_blank" rel="noreferrer">${esc(r.id)}</a> ${route(app, r)}<div class="muted">${esc(r.task.title)}</div>
          ${r.error ? `<div class="kc-error">${errorDetails(r.error)}</div>` : ""}</td>
        <td class="muted small">${day(r.archivedAt!)}</td><td class="act">${actions}</td></tr>`;
    })
    .join("");
  const body = `<div class="board-head"><h1>Архів</h1><div class="archive-tools">${records.length
      ? `<form method="post" action="/delete" data-busy="Видаляю…" data-confirm-title="Видалити весь архів?" data-confirm-ok="Видалити всі (${records.length})" data-confirm="${esc(DELETE_NOTE)}">` +
        `<input type="hidden" name="all" value="1"><input type="hidden" name="token" value="${esc(token)}"><button class="danger-outline">Видалити всі</button></form>`
      : ""}<a class="link" href="/">← До дошки</a></div></div>
    ${rows ? `<table class="tasks archive">${rows}</table>` : `<p class="muted">Архів порожній. На дошці кнопка «В архів» прибирає виконані й невиконані задачі сюди.</p>`}`;
  return page({ title: "Архів", active: "inbox", token, body });
}

const DELETE_NOTE =
  "Mako видалить worktree задачі (незакомічені зміни в ньому зникнуть), локальну гілку й журнал. Задача в трекері, запушена гілка й MR лишаться. Якщо задача й далі підходить під фільтр, Mako запропонує її знову.";

/** Within a column: what needs you first, then the most recent. */
function columnOrder(col: Column["key"]) {
  const time = (r: TaskRecord) => r.finishedAt ?? r.updatedAt;
  const needsYou = (r: TaskRecord) => {
    const s = r.status === "running" ? sessions.get(sessionIdFor(r.task)) : undefined;
    return r.status === "asking" || s?.status === "waiting" || s?.status === "idle" ? 0 : 1;
  };
  // On review, the MRs that need something (CI, conflicts, comments) come first.
  const reviewNeeds = (r: TaskRecord) =>
    r.review && (r.review.pipeline?.status === "failed" || r.review.conflicts || r.review.comments.length) ? 0 : 1;
  return (a: TaskRecord, b: TaskRecord) => {
    if (col === "review") {
      const d = reviewNeeds(a) - reviewNeeds(b);
      if (d) return d;
    }
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
  // With an MR or a finished run, the old worktree already has the answer and the agent would skip the task:
  // start over in a new one. A failed run is usually worth continuing where it stopped.
  const freshByDefault = action === "retry" && (r.status === "done" || Boolean(r.prUrl));
  const quick =
    `<form method="post" action="/${action}">${hidden}${freshByDefault ? `<input type="hidden" name="fresh" value="1">` : ""}` +
    `<button class="${action === "take" ? "primary" : "soft"} take-main" title="${freshByDefault ? "З нуля: новий worktree і нова гілка від основної" : action === "retry" ? "У тому ж worktree, з того місця, де зупинились" : ""}">${verb}</button></form>`;
  const choices = app.orchestrator?.choicesFor(r.id);
  if (!choices) return quick;
  const option = (value: string, label: string, selected: boolean) =>
    `<option value="${esc(value)}"${selected ? " selected" : ""}>${esc(label)}</option>`;
  const executors = [...choices.executors]
    .sort((a, b) => Number(b.name === choices.executor) - Number(a.name === choices.executor))
    .map(
      (e) =>
        `<label class="tp-opt"><input type="radio" name="executor" value="${esc(e.name)}"${e.name === choices.executor ? " checked" : ""}>` +
        `<span>${esc(e.label)}</span>${e.name === choices.executor ? `<span class="badge ok">основний</span>` : ""}</label>`,
    )
    .join("");
  const repos =
    choices.repos.length > 1
      ? `<label>Репозиторій<select name="repo">${choices.repos.map((n) => option(n, n, n === choices.repo)).join("")}</select></label>`
      : "";
  return `<div class="take ${action}">${quick}<details class="take-menu"><summary class="take-more" title="Вибрати, куди відправити задачу">▾</summary>
    <form method="post" action="/${action}" class="take-pop">${hidden}
      <div class="tp-title">${verb}: хто виконує</div>
      <input type="hidden" name="planShown" value="1"><label class="tp-check"><input type="checkbox" name="planFirst" value="1"${planDefault(app, r, action) ? " checked" : ""}><span>Спершу план<small>агент запропонує план, а працювати почне, коли ви його погодите</small></span></label>
      ${action === "retry" ? `<label class="tp-check"><input type="checkbox" name="fresh" value="1"${freshByDefault ? " checked" : ""}><span>З нуля в новому worktree<small>нова гілка від свіжої основної; стара спроба${r.prUrl ? " і її MR" : ""} лишаться</small></span></label>` : ""}
      ${repos}
      <fieldset class="tp-execs">${executors}</fieldset>
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
    r.status === "running" && !session ? `<a class="btn" href="/log?task=${encodeURIComponent(r.id)}">Хід роботи</a>` + takeOverForm(app, r, token) : "",
    r.status === "asking" || r.status === "snoozed" ? takeControl(app, r, token, "take") + form("later", "Відкласти") : "",
    r.status === "planned" && r.plan ? planControls(r, token) : "",
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
    r.summary ? summaryButton(app, r) : "",
    app.orchestrator?.workspaceFor(r.id) ? `<a class="link" href="/changes?task=${encodeURIComponent(r.id)}">Зміни</a>` : "",
    r.prUrl ? `<a class="link" href="${esc(r.prUrl)}" target="_blank" rel="noreferrer">${reviewKind(app, r)} ↗</a>` : "",
    ["done", "failed", "cancelled"].includes(r.status)
      ? `<form method="post" action="/archive" class="kc-archive"><input type="hidden" name="id" value="${esc(r.id)}"><input type="hidden" name="token" value="${esc(token)}"><button class="link muted-link" title="Прибрати з дошки в архів">В архів</button></form>`
      : "",
  ].join("");
  const status = session
    ? `<span class="s ${session.status}">${SESSION_LABELS[session.status]}</span>${session.message && session.status === "waiting" ? ` <span class="muted">${esc(session.message)}</span>` : ""}`
    : columnOf(r) === "review"
    ? `<span class="s review">На ревʼю</span>`
    : `<span class="s ${r.status}">${LABELS[r.status]}</span>${
        r.status === "handed-off" ? ` <span class="muted">чекає PR з <code>${esc(r.branch ?? "")}</code></span>`
        : r.status === "snoozed" && r.snoozedUntil ? ` <span class="muted">до ${new Date(r.snoozedUntil).toLocaleTimeString("uk-UA", { hour: "2-digit", minute: "2-digit" })}</span>`
        : ""
      }`;
  const needsPermission = r.status === "failed" && Boolean(r.denied?.length);
  const trackerName = app.orchestrator?.trackers.get(r.task.trackerId)?.name ?? "трекері";
  const gone = r.gone
    ? `<span class="s gone ${r.gone}" title="${r.gone === "deleted" ? "Задачі більше немає в трекері" : "Задачу закрили в трекері"}">${r.gone === "deleted" ? `🗑 Видалена в ${esc(trackerName)}` : `✓ Закрита в ${esc(trackerName)}`}</span>`
    : "";
  const attn = r.status === "asking" || session?.status === "waiting" || session?.status === "idle" || needsPermission || (r.status === "running" && r.reviewState === "merged");
  const draggable = col === "new" || col === "failed";
  // What the board filter searches: id, title, tracker, repo, executor, labels.
  const haystack = [r.id, r.task.title, trackerName, r.repo, r.executor, ...(r.task.labels ?? [])].join(" ").toLowerCase();
  return `<article class="kcard${attn ? " attn" : ""}" data-search="${esc(haystack)}"${draggable ? ` draggable="true" data-id="${esc(r.id)}" data-from="${col}"` : ""}>
    <div class="kc-top">${r.task.url ? `<a class="kc-id" href="${esc(r.task.url)}" target="_blank" rel="noreferrer" title="${esc(r.id)}">${esc(shortId(r.id))}</a>` : `<span class="kc-id" title="Задача, створена в Mako">${esc(r.id)}</span>`}
      ${route(app, r)}</div>
    <div class="kc-title">${esc(r.task.title)}${attachmentsMark(r)}</div>
    ${description(r)}
    <div class="kc-status">${gone ? (r.status === "cancelled" ? gone : `${status} ${gone}`) : needsPermission ? `<span class="s perm">🔒 Потрібні дозволи</span>` : status}</div>
    ${r.status === "running" && !session ? `<div class="kc-phase muted" data-phase="${esc(r.id)}" title="${esc(phase(recentLog(r.id).at(-1)?.text ?? ""))}">${esc(phase(recentLog(r.id).at(-1)?.text ?? "")) || "&nbsp;"}</div>` : ""}
    ${reviewBlock(app, r, token)}
    ${r.reviewState ? `<div class="kc-review ${r.reviewState}">${r.reviewState === "merged" ? `✓ ${reviewKind(app, r)} змерджено${r.status === "running" ? " · натисніть «Завершити» в терміналі" : ""}` : `${reviewKind(app, r)} закрито без мерджу`}</div>` : ""}
    ${r.trackerStatus ? `<div class="kc-tstatus${r.trackerStatus.ok ? "" : " warn"}" title="${esc(r.trackerStatus.text)}">${r.trackerStatus.ok ? "" : "⚠ "}${esc(r.trackerStatus.text.length > 140 ? r.trackerStatus.text.slice(0, 139) + "…" : r.trackerStatus.text)}${!r.trackerStatus.ok && /Налаштуваннях/.test(r.trackerStatus.text) ? ` <a href="/settings">Вибрати</a>` : ""}</div>` : ""}
    ${needsPermission
      ? `<div class="kc-perm"><div class="small muted">Агенту не дозволили:</div><ul>${r.denied!.slice(0, 3).map((d) => `<li><code>${esc(d)}</code></li>`).join("")}${r.denied!.length > 3 ? `<li class="muted">і ще ${r.denied!.length - 3}</li>` : ""}</ul></div>`
      : r.error && !(r.gone && r.status === "cancelled") ? `<div class="kc-error">${errorDetails(r.error)}</div>` : ""}
    ${actions ? `<div class="kc-actions">${actions}</div>` : ""}
  </article>`;
}

/** The latest step of a running task in a few words. */
function phase(line: string): string {
  const t = line.replace(/\s+/g, " ").trim();
  return t.length > 300 ? t.slice(0, 299) + "…" : t; // the card cuts it to one line; the tooltip has the rest
}

/** What a task without a terminal is doing: Mako's own steps and, for background agents, their actions. */
export function renderTaskLog(app: App, id: string, token: string): string {
  const r = app.orchestrator?.state.get(id);
  const lines = recentLog(id);
  const session = r?.status === "running" ? sessions.get(sessionIdFor(r.task)) : undefined;
  if (session) return page({ title: "Хід роботи", active: "inbox", token, body: "", head: `<meta http-equiv="refresh" content="0;url=/session/${esc(session.id)}">` });
  const last = lines.at(-1);
  const quiet = last ? Math.floor((Date.now() - last.at) / 60_000) : 0;
  const time = (at: number) => new Date(at).toLocaleTimeString("uk-UA", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  const live = r?.status === "running" || r?.status === "queued";
  const body = `<div class="board-head"><h1>Хід роботи</h1><a class="link" href="/">← До дошки</a></div>
    ${r ? `<p><a href="${esc(r.task.url)}" target="_blank" rel="noreferrer">${esc(r.id)}</a> ${esc(r.task.title)} · <span class="s ${r.status}">${LABELS[r.status]}</span></p>` : ""}
    ${r?.status === "running" ? `<div class="tlog-bar"><span class="muted">${last ? `Остання подія ${quiet ? `${quiet} хв тому${quiet >= 3 ? " — агент щось довго думає або чекає на відповідь мережі" : ""}` : "щойно"}` : ""}</span>${takeOverForm(app, r, token, true)}</div>` : ""}
    ${lines.length
      ? `<ol class="tlog">${[...lines].reverse().map((l) => `<li><span class="muted">${time(l.at)}</span> ${esc(l.text)}</li>`).join("")}</ol>`
      : `<p class="muted">Поки що записів немає${live ? "" : " (журнал зберігається, доки Mako запущений)"}.</p>`}
    ${live ? `<p class="small muted tlog-foot">Нові записи зверху, журнал оновлюється сам.</p>` : ""}`;
  return page({ title: `Хід роботи ${id}`, active: "inbox", token, body, wide: true, script: "/ui/tasklog.js", data: { live: live ? "1" : "" } });
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
      <p class="muted small">${r.task.url ? `<a href="${esc(r.task.url)}" target="_blank" rel="noreferrer">${esc(r.id)} ↗</a>` : `${esc(r.id)} · задача з Mako`}</p>
      <div class="desc-body">${renderMarkdown(full, r.task.url)}</div></template>`;
}

/** Markdown without its markup, for a one-paragraph preview. */
function plainText(md: string): string {
  // Rendered first, so only real Markdown markup goes (PRODUCT_WITH_LONG_NAME keeps its underscores).
  return renderMarkdown(md.replace(/```[\s\S]*?```/g, " "), "http://localhost/")
    .replace(/<img[^>]*>/g, " ")
    .replace(/<\/?(em|strong|code|a|del|b|i)\b[^>]*>/g, "") // inline: no gap before punctuation
    .replace(/<[^>]+>/g, " ")
    .replace(/&(amp|lt|gt|quot|#39);/g, (_, e: string) => ({ amp: "&", lt: "<", gt: ">", quot: '"', "#39": "'" })[e]!)
    .replace(/\s+/g, " ")
    .trim();
}


/** A background Claude run, moved into Mako's terminal mid-way: same conversation, now visible and steerable. */
function takeOverForm(app: App, r: TaskRecord, token: string, primary = false): string {
  const terminal = app.orchestrator?.terminalExecutor();
  if (!terminal || r.executor === terminal || !r.sessionId) return "";
  return `<form method="post" action="/takeover" data-confirm-title="Перейти в термінал?" data-confirm-ok="Перейти" data-confirm-tone="primary" data-confirm="Фоновий агент зупиниться, і та сама розмова відкриється в терміналі Mako: ви побачите, що він робить, і зможете дати дозволи."><input type="hidden" name="id" value="${esc(r.id)}"><input type="hidden" name="token" value="${esc(token)}"><button class="${primary ? "primary" : "soft"}">Перейти в термінал</button></form>`;
}

/** "MR" for GitLab tasks, "PR" elsewhere. */
/** «MR» on GitLab, «PR» on GitHub: by the link itself, since a Jira task's review lives on either. */
function reviewKind(app: App, r: TaskRecord): string {
  if (r.prUrl?.includes("/merge_requests/")) return "MR";
  if (r.prUrl?.includes("/pull/")) return "PR";
  return app.orchestrator?.trackers.get(r.task.trackerId)?.name === "GitLab" ? "MR" : "PR";
}

/** «📎 2» after the title: the issue has files, and the agent gets them. */
function attachmentsMark(r: TaskRecord): string {
  const files = r.task.attachments ?? [];
  if (!files.length) return "";
  const list = files.map((a) => `${a.name} (${a.where})`).join("\n");
  return ` <span class="kc-files" title="${esc(`Файли задачі, агент отримає їх:\n${list}`)}">📎 ${files.length}</span>`;
}

/** «group/market-autotests#31» → «market-autotests#31»: the group repeats on every card of a project. */
function shortId(id: string): string {
  return id.replace(/^.*\/(?=[^/]*#)/, "");
}

const BLOCKERS: Record<string, string> = {
  ci_still_running: "CI ще йде",
  ci_must_pass: "CI має пройти",
  not_approved: "чекає апруву",
  discussions_not_resolved: "є невирішені коментарі",
  draft_status: "чернетка",
  conflict: "конфлікти",
  need_rebase: "потрібен rebase",
  checking: "GitLab перевіряє",
  unchecked: "GitLab перевіряє",
  blocked_status: "заблоковано іншим MR",
  requested_changes: "просять змін",
  blocked: "заблоковано правилами",
  behind: "відстає від основної",
  dirty: "конфлікти",
  unstable: "CI не зелений",
};

const PIPELINE: Record<string, [string, string]> = {
  success: ["ok", "✓ CI"],
  failed: ["err", "✗ CI впав"],
  running: ["run", "⏳ CI йде"],
  pending: ["run", "⏳ CI чекає"],
  created: ["run", "⏳ CI"],
  preparing: ["run", "⏳ CI"],
  waiting_for_resource: ["run", "⏳ CI чекає"],
  canceled: ["mute", "CI скасовано"],
  skipped: ["mute", "CI пропущено"],
  manual: ["mute", "CI чекає ручного запуску"],
};

/** The open PR/MR at a glance — CI, threads, conflicts — with the next step one click away. */
function reviewBlock(app: App, r: TaskRecord, token: string): string {
  const review = r.review;
  if (!r.prUrl || !review || review.state !== "open" || r.reviewState) return "";
  const kind = reviewKind(app, r);
  const p = review.pipeline ? [...(PIPELINE[review.pipeline.status] ?? ["mute", `CI: ${review.pipeline.status}`])] : undefined;
  const jobs = review.pipeline?.jobs ?? [];
  if (p && jobs.length && p[0] === "run") {
    // Progress like 3/7: how many jobs have already finished.
    const finished = jobs.filter((j) => ["success", "failed", "canceled", "skipped"].includes(j.status)).length;
    p[1] = `${p[1]} ${finished}/${jobs.length}`;
  }
  if (p && review.pipeline?.status === "failed" && review.pipeline.failed.length) {
    p[1] = `${p[1]}: ${review.pipeline.failed.slice(0, 2).map((j) => j.name).join(", ")}${review.pipeline.failed.length > 2 ? "…" : ""}`;
  }
  const ciRunning = ["running", "pending", "created", "preparing", "waiting_for_resource"].includes(review.pipeline?.status ?? "");
  const facts = [
    p ? `<a class="rv ${p[0]}" href="${esc(review.pipeline!.url ?? r.prUrl)}" target="_blank" rel="noreferrer">${p[1]}</a>` : "",
    review.conflicts ? `<span class="rv err">⚠ конфлікти</span>` : "",
    review.comments.length ? `<a class="rv warn" href="${esc(review.comments[0].url ?? r.prUrl)}" target="_blank" rel="noreferrer">💬 ${review.comments.length}</a>` : "",
    ciRunning && review.mergeable
      ? `<span class="rv mute" title="GitLab дозволяє мердж, бо проєкт не вимагає зеленого CI">можна мерджити, але CI ще йде</span>`
      : review.mergeable ? `<span class="rv ok">готовий до мерджу</span>` : review.blocker && BLOCKERS[review.blocker] && !review.conflicts ? `<span class="rv mute">${BLOCKERS[review.blocker]}</span>` : "",
  ].filter(Boolean).join("");
  const form = (action: string, label: string, cls: string, extra = "", confirm = "") =>
    `<form method="post" action="/${action}"${confirm}><input type="hidden" name="id" value="${esc(r.id)}"><input type="hidden" name="token" value="${esc(token)}">${extra}<button class="${cls}">${label}</button></form>`;
  const ask = (kindName: string, label: string) => form("review-ask", label, "soft", `<input type="hidden" name="kind" value="${kindName}">`);
  const buttons = [
    review.pipeline?.status === "failed" ? ask("ci", "Полагодити CI") : "",
    review.comments.length ? ask("comments", `Виправити коментарі (${review.comments.length})`) : "",
    review.conflicts ? ask("conflicts", "Розвʼязати конфлікти") : "",
    review.mergeable && ciRunning
      ? form("review-merge", "Змерджити, не чекаючи CI", "soft", "", ` data-confirm-title="Змерджити, поки CI ще йде?" data-confirm-ok="Змерджити" data-confirm-tone="danger" data-confirm="${esc(`CI ще не закінчився: якщо він впаде, зламаний код уже буде в основній гілці. ${kind} буде змерджено, гілку задачі видалено, задача закриється.`)}"`)
      : review.mergeable
      ? form("review-merge", "Змерджити", "success", "", ` data-confirm-title="Змерджити ${kind}?" data-confirm-ok="Змерджити" data-confirm-tone="success" data-confirm="${esc(`${kind} буде змерджено в основну гілку, гілку задачі буде видалено. Задача отримає фінальний статус і закриється.`)}"`)
      : "",
  ].filter(Boolean).join("");
  return `<div class="kc-mr"><div class="kc-mr-facts"><span class="muted">${kind}:</span>${facts || `<span class="rv mute">перевіряю…</span>`}</div>${buttons ? `<div class="kc-mr-actions">${buttons}</div>` : ""}${autopilotLine(app, r, form)}</div>`;
}

/** The MR autopilot on the card: what it is doing, and a switch for this task. */
function autopilotLine(app: App, r: TaskRecord, form: (action: string, label: string, cls: string, extra?: string) => string): string {
  const o = app.orchestrator;
  if (!o) return "";
  const on = o.reviews.autopilotOn(r);
  const toggle = form("autopilot", on ? "Вимкнути" : "Увімкнути автопілот", "link", `<input type="hidden" name="on" value="${on ? "0" : "1"}">`);
  if (!on) return `<div class="kc-pilot off">${toggle}</div>`;
  const doing = r.autopilotNote ?? (r.status === "done" ? "стежить за MR" : "чекає, поки агент закінчить");
  return `<div class="kc-pilot" title="Автопілот сам віддає агенту впалий CI, нові коментарі й конфлікти, не більше ${o.cfg.autopilot.maxRounds} раундів">🤖 <span><b>Автопілот</b> · ${esc(doing)}</span>${toggle}</div>`;
}

/** «Підсумок»: the summary on the left and, while the worktree exists, its changes on the right. */
function summaryButton(app: App, r: TaskRecord): string {
  const when = r.finishedAt ? ` · ${new Date(r.finishedAt).toLocaleString("uk-UA", { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" })}` : "";
  const text =
    `<h2>Підсумок · ${esc(r.task.title)}</h2><p class="muted small">${r.task.url ? `<a href="${esc(r.task.url)}" target="_blank" rel="noreferrer">${esc(r.id)} ↗</a>` : esc(r.id)}` +
    `${r.prUrl ? ` · <a href="${esc(r.prUrl)}" target="_blank" rel="noreferrer">${reviewKind(app, r)} ↗</a>` : ""}${when}</p>` +
    `<div class="desc-body">${renderMarkdown(r.summary!, r.task.url)}</div>`;
  const changes = app.orchestrator?.workspaceFor(r.id)
    ? `<iframe class="sum-changes" title="Зміни" src="/changes?task=${encodeURIComponent(r.id)}&amp;embed=1"></iframe>`
    : "";
  return (
    `<button type="button" class="link" data-desc title="Що зроблено, як перевірено, на що звернути увагу">Підсумок</button>` +
    `<template class="kc-full"${changes ? ` data-wide="1"` : ""}>${changes ? `<div class="sum-grid"><div class="sum-text">${text}</div>${changes}</div>` : text}</template>`
  );
}

/** «План готовий»: read the plan, then approve it, ask for changes, or drop the task. */
function planControls(r: TaskRecord, token: string): string {
  const hidden = `<input type="hidden" name="id" value="${esc(r.id)}"><input type="hidden" name="token" value="${esc(token)}">`;
  const answer = (value: string, label: string, cls: string, extra = "") =>
    `<form method="post" action="/plan"${extra}>${hidden}<input type="hidden" name="answer" value="${value}"><button class="${cls}">${label}</button></form>`;
  const dialog =
    `<h2>План · ${esc(r.task.title)}</h2><div class="desc-body">${renderMarkdown(r.plan!.replace(/^\s*#{1,3}\s*План\s*\n/i, ""), r.task.url || "http://localhost/")}</div>` +
    `<form method="post" action="/plan" class="plan-revise">${hidden}<input type="hidden" name="answer" value="revise">` +
    `<label class="f">Що змінити в плані<textarea name="notes" rows="3" placeholder="Напр.: не чіпай API, спершу напиши тест, який відтворює баг" required></textarea></label>` +
    `<button class="soft">Переплануй з урахуванням</button></form>`;
  return (
    `<button type="button" class="primary" data-desc>Переглянути план</button><template class="kc-full">${dialog}</template>` +
    answer("approve", "Погодити", "success") +
    answer("reject", "Відхилити", "danger-outline", ` data-confirm-title="Відхилити план?" data-confirm-ok="Відхилити" data-confirm="Задача перейде в «Не виконано», агент її не робитиме."`)
  );
}

/** «+ Задача»: a task for the agent without an issue anywhere. */
function newTaskDialog(app: App, token: string): string {
  const repos = Object.keys(app.orchestrator?.trackers.get("mako")?.projects ?? {});
  const choices = app.orchestrator?.executorChoices() ?? [];
  const templates = app.orchestrator?.templateNames() ?? [];
  if (!repos.length) return "";
  return `<dialog id="new-task" class="mdialog wide-form"><form method="post" action="/create-task">
    <input type="hidden" name="token" value="${esc(token)}">
    <h2>Нова задача для агента</h2>
    <label class="f">Що зробити<input type="text" name="title" required placeholder="Напр.: додай тест на оформлення замовлення" autocomplete="off"></label>
    <label class="f">Подробиці<textarea name="description" rows="5" placeholder="Контекст, посилання, критерії готовності (Markdown)"></textarea></label>
    ${templates.length ? `<label class="f">Шаблон<select name="template"><option value="">без шаблону (або за назвою задачі)</option>${templates.map((t) => `<option>${esc(t)}</option>`).join("")}</select></label>` : ""}
    <div class="grid">
      <label class="f">Репозиторій<select name="repo">${repos.map((r) => `<option>${esc(r)}</option>`).join("")}</select></label>
      <label class="f">Хто виконує<select name="executor">${choices.map((e) => `<option value="${esc(e.name)}"${e.default ? " selected" : ""}>${esc(e.label)}${e.default ? " · основний" : ""}</option>`).join("")}</select></label>
    </div>
    <input type="hidden" name="planShown" value="1"><label class="check"><input type="checkbox" name="planFirst" value="1"${app.cfg?.planFirst !== false ? " checked" : ""}> Спершу план: агент запропонує, працювати почне після вашого погодження</label>
    <div class="mactions"><button type="button" class="mcancel" data-close>Скасувати</button><button class="primary">Взяти в роботу</button></div>
  </form></dialog>`;
}

/** The «Спершу план» checkbox as it starts: the setting for new work; a retry in the same worktree keeps an approved plan. */
function planDefault(app: App, r: TaskRecord, action: "take" | "retry"): boolean {
  if (app.cfg?.planFirst === false) return false;
  return action === "take" || !r.planApproved;
}
