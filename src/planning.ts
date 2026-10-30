import type { RepoConfig, TaskTemplate } from "./config.js";
import type { Tracker } from "./types.js";
import type { TaskRecord } from "./state.js";
import type { Orchestrator } from "./orchestrator.js";
import * as git from "./git.js";
import { makePlan } from "./plan.js";
import { readMemory, writeMemory } from "./memory.js";
import { LOCAL_TRACKER } from "./trackers/local.js";
import { inform } from "./notify.js";
import { log } from "./log.js";

/** What goes into an agent's prompt besides the task (templates, repo notes) and the plan before the work. */
export class Planning {
  constructor(private o: Orchestrator) {}

  /** Templates that fit the task (by labels, title, or chosen in «+ Задача»). */
  templatesFor(rec: TaskRecord): TaskTemplate[] {
    const labels = rec.task.labels.map((l) => l.toLowerCase());
    const title = rec.task.title.toLowerCase();
    return this.o.cfg.templates.filter(
      (t) =>
        t.name === rec.template ||
        t.labels.some((l) => labels.includes(l)) ||
        t.titleContains.some((w) => w && title.includes(w)),
    );
  }

  /** What Mako adds to the task's own text: matching templates and what earlier tasks learned about the repo. */
  promptContext(rec: TaskRecord, repo: RepoConfig): string {
    const parts: string[] = [];
    for (const t of this.templatesFor(rec)) {
      log(rec.id, `template: ${t.name}`);
      parts.push(`## Instructions for this kind of task («${t.name}»)\n\n${t.instructions}`);
    }
    const memory = this.o.cfg.memory ? readMemory(this.o.cfg.stateDir, repo.name) : "";
    if (memory) parts.push(`## What earlier tasks learned about this repository\n\nNotes kept by Mako from previous agents' work here. Use them; they may be out of date.\n\n${memory}`);
    return parts.length ? `\n\n${parts.join("\n\n")}` : "";
  }

  isTerminalClaude(executor: string): boolean {
    const c: any = this.o.cfg.executors[executor];
    return c?.type === "terminal" && /^claude(\.cmd|\.exe)?$/i.test(String(c.command ?? "claude").split(/[\\/]/).pop()!);
  }

  /** Claude reads the code and proposes a plan; the task waits as «План готовий» for the developer. */
  async planPhase(rec: TaskRecord, tracker: Tracker, repo: RepoConfig, branch: string, prompt: string): Promise<void> {
    const { task } = rec;
    this.o.state.update(task.id, { status: "running", startedAt: new Date().toISOString(), repo: repo.name, branchName: branch });
    try {
      const worktree = await git.prepareWorktree(repo, task.slug, (line) => log(task.id, line), branch);
      log(task.id, rec.planFeedback ? "replanning with the developer's notes" : "Claude is writing a plan");
      const ask = rec.planFeedback && rec.plan
        ? `${prompt}\n\n## Your previous plan\n\n${rec.plan}\n\n## What the developer wants changed\n\n${rec.planFeedback}`
        : prompt;
      const plan = await makePlan(worktree, ask);
      this.o.state.update(task.id, { status: "planned", plan, planFeedback: undefined, worktree });
      log(task.id, "plan ready: waiting for the developer");
      inform(`📝 ${task.id}: план готовий`, "Перегляньте й погодьте його на дошці Mako");
    } catch (err: any) {
      log(task.id, `plan failed: ${err.message}`);
      this.o.state.update(task.id, { status: "failed", finishedAt: new Date().toISOString(), error: `План не складено: ${err.message}` });
    }
  }

  /** The developer's answer to a plan: go, plan again with notes, or drop the task. */
  answerPlan(id: string, answer: "approve" | "revise" | "reject", notes = ""): boolean {
    const rec = this.o.state.get(id);
    if (!rec || rec.status !== "planned") return false;
    if (answer === "approve") {
      log(id, "plan approved");
      this.o.state.update(id, { status: "queued", queuedAt: Date.now(), planApproved: true });
    } else if (answer === "revise") {
      if (!notes.trim()) return false;
      log(id, `plan to be revised: ${notes.trim().slice(0, 120)}`);
      this.o.state.update(id, { status: "queued", queuedAt: Date.now(), planFeedback: notes.trim() });
    } else {
      log(id, "plan rejected");
      this.o.state.update(id, { status: "cancelled", finishedAt: new Date().toISOString(), error: "План відхилено" });
    }
    this.o.schedule();
    return true;
  }

  /** The repos Mako keeps notes for, with their notes. */
  memories(): { repo: string; text: string }[] {
    return Object.keys(this.o.trackers.get(LOCAL_TRACKER)!.projects).map((repo) => ({ repo, text: readMemory(this.o.cfg.stateDir, repo) }));
  }

  saveMemory(repo: string, text: string): void {
    if (!this.o.trackers.get(LOCAL_TRACKER)!.projects[repo]) throw new Error(`Немає репозиторію «${repo}»`);
    writeMemory(this.o.cfg.stateDir, repo, text);
    log("main", `repo notes ${text.trim() ? "edited" : "cleared"}: ${repo}`);
  }

  /** Templates, for «+ Задача». */
  templateNames(): string[] {
    return this.o.cfg.templates.map((t) => t.name);
  }
}
