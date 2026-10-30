import type { Orchestrator } from "./orchestrator.js";
import type { Task, Tracker } from "./types.js";
import type { TaskRecord } from "./state.js";
import { routeTask } from "./routing.js";
import { log } from "./log.js";

/** What Mako does with an issue of the tracker, in words for «Трекер». */
export interface MakoView {
  /** board: on the board; offered: will show up after the next search; skipped: Mako leaves it out. */
  kind: "board" | "archived" | "offered" | "skipped";
  text: string;
  record?: TaskRecord;
}

export interface TrackerView {
  tracker: Tracker;
  me?: string;
  filter?: string;
  items: { task: Task; mako: MakoView }[];
  error?: string;
}

/**
 * Every open issue of the configured projects, with where it stands for Mako: on the board, coming with the next
 * search, or left out and why. The tracker's own search is run once to tell its filters apart.
 */
export async function browseTrackers(o: Orchestrator): Promise<TrackerView[]> {
  const views: TrackerView[] = [];
  for (const tracker of o.trackers.values()) {
    if (!tracker.browseTasks) continue;
    try {
      const [tasks, inQuery, me, filter] = await Promise.all([
        tracker.browseTasks(),
        tracker.fetchTasks().then((ts) => new Set(ts.map((t) => t.id))),
        tracker.whoAmI?.().catch(() => undefined),
        tracker.describeFilter?.().catch(() => undefined),
      ]);
      const skipOthers = (o.cfg.trackers as any[]).find((t) => (t.id ?? t.type) === tracker.id)?.skipOthersTasks !== false;
      const items = tasks.map((task) => ({ task, mako: makoView(o, tracker, task, inQuery.has(task.id), skipOthers ? me : undefined) }));
      views.push({ tracker, me, filter, items });
    } catch (err: any) {
      views.push({ tracker, items: [], error: err.message });
    }
  }
  return views;
}

function makoView(o: Orchestrator, tracker: Tracker, task: Task, inQuery: boolean, me?: string): MakoView {
  const rec = o.state.get(task.id);
  if (rec?.archivedAt) return { kind: "archived", text: "в архіві", record: rec };
  if (rec) return { kind: "board", text: STATUS[rec.status] ?? rec.status, record: rec };
  if (!inQuery) return { kind: "skipped", text: "не підходить під фільтр Mako" };
  const owners = (task.assignees ?? []).map((a) => a.toLowerCase());
  if (me && owners.length && !owners.includes(me.toLowerCase())) return { kind: "skipped", text: "призначена на інших" };
  o.ensureProject(tracker, task.project);
  const project = tracker.projects[task.project];
  const route = project ? routeTask(task, project) : undefined;
  if (!route?.repo) return { kind: "skipped", text: `немає репозиторію${route ? `: ${route.reason}` : ""}` };
  return { kind: "offered", text: "зʼявиться після наступного пошуку" };
}

const STATUS: Record<string, string> = {
  asking: "на дошці: нова",
  snoozed: "на дошці: нова",
  queued: "в роботі (черга)",
  planned: "в роботі (план)",
  running: "в роботі",
  "handed-off": "в роботі (передано)",
  done: "готово",
  failed: "не виконано",
  cancelled: "скасовано",
};

/**
 * «Взяти» from «Трекер»: an issue Mako's filters left out goes to work anyway, read in full from the tracker
 * (description, comments) and routed like any other.
 */
export async function adoptTask(o: Orchestrator, ref: string): Promise<string> {
  for (const tracker of o.trackers.values()) {
    const found = await tracker.inspectTask?.(ref);
    if (!found) continue;
    const { task } = found;
    const rec = o.state.get(task.id);
    if (rec && !rec.archivedAt) throw new Error(`${task.id} уже на дошці`);
    if (rec?.archivedAt) {
      // Back from the archive: «Повторити» starts it over.
      o.state.update(task.id, { archivedAt: undefined });
      if (!o.retry(task.id, { fresh: true })) throw new Error(`${task.id} зараз не можна повторити`);
      return task.id;
    }
    o.ensureProject(tracker, task.project);
    const project = tracker.projects[task.project];
    const route = project ? routeTask(task, project) : undefined;
    if (!route?.repo) throw new Error(`Для ${task.id} немає репозиторію: ${route?.reason ?? "проєкт не доданий у Налаштуваннях"}`);
    log(task.id, `taken from «Трекер» → ${route.repo.name}: ${task.title}`);
    o.state.update(task.id, { task, repo: route.repo.name, status: "snoozed", snoozedUntil: 0 });
    o.decide(task.id, "take");
    return task.id;
  }
  throw new Error("Не впізнав задачу");
}
