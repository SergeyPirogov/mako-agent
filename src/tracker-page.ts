import type { App } from "./app.js";
import { browseTrackers, type TrackerView } from "./browse.js";
import { esc, page } from "./ui-layout.js";

/** «Трекер»: every open issue of the configured projects, who it is assigned to, and what Mako does with it. */
export async function renderTracker(app: App, token: string, notice?: string): Promise<string> {
  const o = app.orchestrator;
  if (!o) return page({ title: "Трекер", active: "tracker", token, body: `<h1>Трекер</h1><div class="notice">${esc(app.problem ?? "Mako запускається…")}</div>` });
  const views = await browseTrackers(o);
  const total = views.reduce((n, v) => n + v.items.length, 0);
  const filters = `<div class="tr-filters">
      <input type="search" id="tr-search" class="board-filter" placeholder="Пошук: номер, назва, людина, мітка" aria-label="Пошук задач у трекері" autocomplete="off">
      <select id="tr-who" aria-label="На кого призначена">
        <option value="">усі задачі</option><option value="me">призначені на мене</option>
        <option value="none">без виконавця</option><option value="others">призначені на інших</option>
      </select>
      <label class="check"><input type="checkbox" id="tr-hide-board"> сховати ті, що вже в Mako</label>
      <span class="muted small" id="tr-count">${total} задач</span>
    </div>`;
  const body = `<div class="board-head"><h1>Трекер</h1><a class="link" href="/">← До дошки</a></div>
    <p class="muted">Усі відкриті задачі проєктів із Налаштувань, без фільтрів Mako: на кого призначені і що з ними робить Mako.
      Задачу, яку Mako не пропонує, можна взяти звідси.</p>
    ${notice ? `<div class="notice warn">${esc(notice)}</div>` : ""}
    ${filters}${views.map((v) => section(v, token)).join("") || `<p class="muted">Немає трекерів, які вміють показати свої задачі.</p>`}`;
  return page({ title: "Трекер", active: "tracker", token, body, script: "/ui/tracker.js", wide: true });
}

function section(v: TrackerView, token: string): string {
  const head = `<h2>${esc(v.tracker.name)}${v.me ? ` <span class="muted small">· ви: ${esc(v.me)}</span>` : ""}</h2>
    ${v.filter ? `<p class="muted small">Mako бере: ${esc(v.filter)}</p>` : ""}`;
  if (v.error) return `<section class="tr-section">${head}<div class="notice err">${esc(v.error)}</div></section>`;
  if (!v.items.length) return `<section class="tr-section">${head}<p class="muted">Відкритих задач немає.</p></section>`;
  const me = v.me?.toLowerCase();
  const rows = v.items
    .map(({ task, mako }) => {
      const ids = task.assignees ?? [];
      const names = task.assigneeNames ?? ids;
      const mine = Boolean(me && ids.some((a) => a.toLowerCase() === me));
      const who = !ids.length ? "none" : mine ? "me" : "others";
      const people = names.length
        ? names.map((n, i) => `<span class="chip${me && ids[i]?.toLowerCase() === me ? " me" : ""}">${esc(n)}${me && ids[i]?.toLowerCase() === me ? " (ви)" : ""}</span>`).join("")
        : `<span class="muted">—</span>`;
      const labels = task.labels.map((l) => `<span class="chip label">${esc(l)}</span>`).join("");
      const action =
        mako.kind === "board"
          ? `<a class="link" href="/#${encodeURIComponent(task.id)}">на дошці</a>`
          : `<form method="post" action="/adopt" data-busy="Беру…"><input type="hidden" name="token" value="${esc(token)}"><input type="hidden" name="ref" value="${esc(task.url)}">` +
            `<button class="soft">${mako.kind === "archived" ? "Ще раз" : "Взяти"}</button></form>`;
      const search = [task.id, task.title, ...names, ...task.labels].join(" ").toLowerCase();
      return `<tr data-who="${who}" data-kind="${mako.kind}" data-search="${esc(search)}">
        <td class="tr-task"><a href="${esc(task.url)}" target="_blank" rel="noreferrer">${esc(task.id)}</a> ${esc(task.title)}${labels ? `<div class="tr-labels">${labels}</div>` : ""}</td>
        <td class="tr-people">${people}</td>
        <td class="tr-mako"><span class="tr-state ${mako.kind}">${esc(mako.text)}</span></td>
        <td class="act">${action}</td></tr>`;
    })
    .join("");
  return `<section class="tr-section">${head}
    <table class="tasks tr-table"><thead><tr><th>Задача</th><th>Виконавці</th><th>Mako</th><th></th></tr></thead><tbody>${rows}</tbody></table></section>`;
}
