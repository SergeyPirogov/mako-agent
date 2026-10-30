import type { App } from "./app.js";
import type { TaskRecord } from "./state.js";
import { esc, page } from "./ui-layout.js";

const DAY = 24 * 3600_000;

/** Statistics over every task Mako has seen, archived ones included: throughput, times, cost, failures. */
export function renderStats(app: App, token: string, days: number): string {
  const all = app.orchestrator?.state.all() ?? [];
  const since = days ? Date.now() - days * DAY : 0;
  const at = (s?: string) => (s ? Date.parse(s) : NaN);
  const taken = all.filter((r) => r.startedAt && at(r.startedAt) >= since);
  const finished = all.filter((r) => r.finishedAt && at(r.finishedAt) >= since);
  const done = finished.filter((r) => r.status === "done");
  const failed = finished.filter((r) => r.status === "failed" || r.status === "cancelled");
  const merged = all.filter((r) => r.mergedAt && at(r.mergedAt) >= since);
  const rate = done.length + failed.length ? Math.round((100 * done.length) / (done.length + failed.length)) : undefined;
  const cost = finished.reduce((sum, r) => sum + (r.costUsd ?? 0), 0);
  const toDone = median(done.map((r) => at(r.finishedAt) - at(r.startedAt)));
  const toMerge = median(merged.map((r) => at(r.mergedAt) - at(r.startedAt)));

  const tile = (value: string, label: string, hint = "") =>
    `<div class="st-tile"><div class="st-value">${value}</div><div class="st-label">${label}</div>${hint ? `<div class="st-hint muted">${hint}</div>` : ""}</div>`;
  const tiles = [
    tile(String(taken.length), "взято в роботу"),
    tile(String(done.length), "зроблено", merged.length ? `змерджено: ${merged.length}` : ""),
    tile(rate === undefined ? "—" : `${rate}%`, "вдалих", failed.length ? `не вдалось: ${failed.length}` : ""),
    tile(toDone === undefined ? "—" : duration(toDone), "від «Взяти» до готово", "медіана"),
    tile(toMerge === undefined ? "—" : duration(toMerge), "від «Взяти» до мерджу", "медіана"),
    tile(cost ? `$${cost.toFixed(2)}` : "—", "вартість", cost ? "фонові запуски Claude" : "відома лише для фонових запусків"),
  ].join("");

  const periods = [
    [7, "7 днів"],
    [30, "30 днів"],
    [0, "увесь час"],
  ] as const;
  const tabs = periods.map(([d, label]) => `<a href="/stats?days=${d}" class="${d === days ? "on" : ""}">${label}</a>`).join("");

  const body = `<div class="board-head"><h1>Статистика</h1><nav class="st-periods" aria-label="Період">${tabs}</nav><a class="link" href="/">← До дошки</a></div>
    <div class="st-tiles">${tiles}</div>
    <section class="card st-card"><h2>Задачі по тижнях</h2>${weekly(all)}</section>
    <div class="st-tables">
      <section class="card st-card"><h2>За репозиторієм</h2>${breakdown(finished, (r) => r.repo ?? "—")}</section>
      <section class="card st-card"><h2>За виконавцем</h2>${breakdown(finished, (r) => r.executor ?? "—")}</section>
    </div>
    <section class="card st-card"><h2>Чому не вдалось</h2>${reasons(failed)}</section>`;
  return page({ title: "Статистика", active: "stats", token, body });
}

/** Done and not done per week for the last 8 weeks: a stacked bar each, hover for the numbers. */
function weekly(all: TaskRecord[]): string {
  const weekStart = (t: number) => {
    const d = new Date(t);
    d.setHours(0, 0, 0, 0);
    d.setDate(d.getDate() - ((d.getDay() + 6) % 7)); // Monday
    return d.getTime();
  };
  const now = weekStart(Date.now());
  const weeks = Array.from({ length: 8 }, (_, i) => now - (7 - i) * 7 * DAY);
  const rows = weeks.map((w) => {
    const inWeek = all.filter((r) => r.finishedAt && weekStart(Date.parse(r.finishedAt)) === w);
    return {
      week: w,
      done: inWeek.filter((r) => r.status === "done").length,
      failed: inWeek.filter((r) => r.status === "failed" || r.status === "cancelled").length,
    };
  });
  const max = Math.max(1, ...rows.map((r) => r.done + r.failed));
  const label = (w: number) => new Date(w).toLocaleDateString("uk-UA", { day: "2-digit", month: "2-digit" });
  const bars = rows
    .map((r) => {
      const total = r.done + r.failed;
      const tip = `Тиждень з ${label(r.week)}: зроблено ${r.done}, не вдалось ${r.failed}`;
      return `<div class="st-col" title="${esc(tip)}" tabindex="0" aria-label="${esc(tip)}">
        <div class="st-num">${total || ""}</div>
        <div class="st-bar">${r.failed ? `<span class="seg failed" style="height:${(100 * r.failed) / max}%"></span>` : ""}${r.done ? `<span class="seg done" style="height:${(100 * r.done) / max}%"></span>` : ""}</div>
        <div class="st-week muted">${label(r.week)}</div></div>`;
    })
    .join("");
  const table = rows
    .map((r) => `<tr><td>${label(r.week)}</td><td>${r.done}</td><td>${r.failed}</td></tr>`)
    .join("");
  return `<div class="st-legend"><span><i class="dot done"></i>зроблено</span><span><i class="dot failed"></i>не вдалось</span></div>
    <div class="st-chart" role="img" aria-label="Зроблені й невдалі задачі за останні 8 тижнів">${bars}</div>
    <details class="st-table"><summary>Таблицею</summary><table class="tasks"><tr><th>Тиждень з</th><th>Зроблено</th><th>Не вдалось</th></tr>${table}</table></details>`;
}

function breakdown(records: TaskRecord[], key: (r: TaskRecord) => string): string {
  const groups = new Map<string, { done: number; failed: number }>();
  for (const r of records) {
    const g = groups.get(key(r)) ?? { done: 0, failed: 0 };
    if (r.status === "done") g.done++;
    else if (r.status === "failed" || r.status === "cancelled") g.failed++;
    groups.set(key(r), g);
  }
  if (!groups.size) return `<p class="muted">За цей період ще нічого не завершено.</p>`;
  const rows = [...groups.entries()]
    .sort((a, b) => b[1].done + b[1].failed - (a[1].done + a[1].failed))
    .map(([name, g]) => {
      const rate = Math.round((100 * g.done) / Math.max(1, g.done + g.failed));
      return `<tr><td>${esc(name)}</td><td>${g.done}</td><td>${g.failed}</td><td>${rate}%</td></tr>`;
    })
    .join("");
  return `<table class="tasks st-break"><tr><th></th><th>Зроблено</th><th>Не вдалось</th><th>Вдалих</th></tr>${rows}</table>`;
}

/** The most common first lines of errors, so repeated causes (permissions, CI, setup) stand out. */
function reasons(failed: TaskRecord[]): string {
  const counts = new Map<string, number>();
  for (const r of failed) {
    const first = (r.denied?.length ? "Агенту бракувало дозволів" : (r.error ?? "без пояснення").split("\n")[0])
      .replace(/#\d+|[0-9a-f]{7,}|\d+/g, "…")
      .slice(0, 140);
    counts.set(first, (counts.get(first) ?? 0) + 1);
  }
  if (!counts.size) return `<p class="muted">Невдалих задач за цей період немає.</p>`;
  return `<table class="tasks">${[...counts.entries()]
    .sort((a, b) => b[1] - a[1])
    .slice(0, 8)
    .map(([reason, n]) => `<tr><td>${esc(reason)}</td><td class="st-n">${n}</td></tr>`)
    .join("")}</table>`;
}

function median(values: number[]): number | undefined {
  const v = values.filter((x) => Number.isFinite(x) && x >= 0).sort((a, b) => a - b);
  if (!v.length) return undefined;
  const mid = Math.floor(v.length / 2);
  return v.length % 2 ? v[mid] : (v[mid - 1] + v[mid]) / 2;
}

function duration(ms: number): string {
  const min = Math.round(ms / 60_000);
  if (min < 60) return `${min} хв`;
  const h = Math.floor(min / 60);
  if (h < 48) return `${h} год${min % 60 ? ` ${min % 60} хв` : ""}`;
  return `${Math.round(h / 24)} дн`;
}
