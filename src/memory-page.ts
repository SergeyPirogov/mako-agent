import type { App } from "./app.js";
import { esc, page } from "./ui-layout.js";

/** The notes Mako keeps per repo, to read, fix or clear. */
export function renderMemory(app: App, token: string, saved?: string): string {
  const repos = app.orchestrator?.memories() ?? [];
  const off = app.cfg?.memory === false;
  const cards = repos
    .map(
      ({ repo, text }) => `<form method="post" action="/memory" class="card mem-card">
        <input type="hidden" name="token" value="${esc(token)}"><input type="hidden" name="repo" value="${esc(repo)}">
        <div class="head"><h2>${esc(repo)}</h2>${saved === repo ? `<span class="badge ok">збережено</span>` : ""}</div>
        <textarea name="text" rows="${Math.min(24, Math.max(6, text.split("\n").length + 2))}" placeholder="Поки порожньо: нотатки зʼявляться після першої завершеної задачі в цьому репозиторії. Можна написати й самому, наприклад як запускати тести.">${esc(text)}</textarea>
        <div class="adds"><button class="primary">Зберегти</button><span class="muted small">Порожнє поле очищає памʼять.</span></div>
      </form>`,
    )
    .join("");
  const body = `<div class="board-head"><h1>Памʼять репозиторіїв</h1><a class="link" href="/settings">← Налаштування</a></div>
    <p class="muted">Що агенти дізналися про кожен репозиторій: як запускати тести, структура, домовленості, підводні камені.
    Mako оновлює це після кожної завершеної задачі і дає наступному агенту в тому ж репозиторії.</p>
    ${off ? `<div class="notice warn">Памʼять вимкнена в Налаштуваннях: нові нотатки не збираються й не передаються агентам.</div>` : ""}
    ${cards || `<p class="muted">Репозиторіїв ще немає.</p>`}`;
  return page({ title: "Памʼять репозиторіїв", active: "settings", token, body });
}
