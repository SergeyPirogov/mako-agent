import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { parseConfig } from "../src/config.ts";
import { Orchestrator } from "../src/orchestrator.ts";
import { StateStore } from "../src/state.ts";
import { setNotificationHost } from "../src/notify.ts";
import type { IssueState, Outcome, ReviewInfo, Task, Tracker } from "../src/types.ts";

// The whole loop on real git repos in a temp folder: a tracker in memory, a shell script as the agent.
let dir: string;
const git = (cwd: string, ...args: string[]) => execFileSync("git", args, { cwd, encoding: "utf8" }).trim();
const notices: string[] = [];

class FakeTracker implements Tracker {
  readonly id = "gitlab";
  readonly name = "GitLab";
  issues: Task[] = [];
  state = new Map<string, IssueState>();
  review: ReviewInfo["state"] = "open";
  calls: string[] = [];
  constructor(readonly projects: Tracker["projects"]) {}
  async fetchTasks() {
    return this.issues.filter((t) => (this.state.get(t.id) ?? "open") === "open");
  }
  async comment(task: Task, text: string) {
    this.calls.push(`comment ${task.id}: ${text.split("\n")[0]}`);
  }
  async onStart(task: Task) {
    this.calls.push(`start ${task.id}`);
    return "In progress";
  }
  async onFinish(task: Task, outcome: Outcome) {
    this.calls.push(`finish ${task.id} ${outcome}`);
  }
  async issueState(task: Task) {
    return this.state.get(task.id) ?? "open";
  }
  async reviewDetails(): Promise<ReviewInfo> {
    return { state: this.review, conflicts: false, mergeable: this.review === "open", comments: [], checkedAt: Date.now() };
  }
  async whoAmI() {
    return "me";
  }
  async assignToMe(task: Task) {
    this.calls.push(`assign ${task.id}`);
  }
  async onMerged(task: Task) {
    this.calls.push(`merged ${task.id}`);
    return "Done";
  }
}

const issue = (n: number, title: string): Task => ({
  id: `acme/web#${n}`,
  slug: `web-${n}`,
  trackerId: "gitlab",
  project: "acme/web",
  title,
  description: "Make it so",
  url: `https://git.example/acme/web/-/issues/${n}`,
  labels: [],
  components: [],
  comments: [],
});

function setup(agentScript: string) {
  const base = mkdtempSync(path.join(dir, "case-"));
  const remote = path.join(base, "remote.git");
  const repo = path.join(base, "web");
  execFileSync("git", ["init", "-q", "--bare", "-b", "main", remote]);
  execFileSync("git", ["clone", "-q", remote, repo]);
  git(repo, "config", "user.email", "t@t");
  git(repo, "config", "user.name", "Test");
  writeFileSync(path.join(repo, "README.md"), "# web\n");
  git(repo, "add", ".");
  git(repo, "commit", "-qm", "init");
  git(repo, "push", "-q", "origin", "main");
  const agent = path.join(base, "agent.sh");
  writeFileSync(agent, `#!/bin/sh\n${agentScript}\n`, { mode: 0o755 });
  const cfg = parseConfig({
    stateDir: path.join(base, "state"),
    wipLimit: 2,
    handoffCheckMin: 0,
    branchPattern: "{type}/{id}-{title}",
    notifications: { enabled: false, autoTake: true, inboxPort: 0 },
    executors: { agent: { type: "command", command: agent, mode: "managed" } },
    defaultExecutor: "agent",
    trackers: [{ type: "gitlab", projects: { "acme/web": { repoPath: repo, baseBranch: "main", pullRequest: "none", setup: "none" } } }],
  });
  const tracker = new FakeTracker(cfg.trackers[0].projects);
  const orchestrator = new Orchestrator(cfg, [tracker], new StateStore(cfg.stateDir));
  return { remote, repo, tracker, orchestrator };
}

before(() => {
  dir = mkdtempSync(path.join(os.tmpdir(), "mako-test-"));
  // No real Claude for commit messages and summaries: the fallbacks are used.
  process.env.PATH = "/usr/bin:/bin";
  process.env.HOME = dir;
  setNotificationHost({ ask: async () => "take", inform: (title) => void notices.push(title) });
});
after(() => rmSync(dir, { recursive: true, force: true }));

test("a task goes from the tracker through the agent to a pushed branch", async () => {
  const { remote, tracker, orchestrator } = setup('echo "hello" > hello.txt; echo "Added hello.txt"');
  tracker.issues = [issue(7, "Add a hello file")];
  await orchestrator.poll();
  await orchestrator.drain();

  const rec = orchestrator.state.get("acme/web#7")!;
  assert.equal(rec.status, "done", rec.error);
  assert.equal(rec.branchName, "feat/7-add-a-hello-file");
  assert.match(git(remote, "branch", "--list"), /feat\/7-add-a-hello-file/);
  assert.equal(git(remote, "show", "feat/7-add-a-hello-file:hello.txt"), "hello");
  assert.deepEqual(tracker.calls.filter((c) => !c.startsWith("comment") && !c.startsWith("assign")), ["start acme/web#7", "finish acme/web#7 success"]);
  assert.ok(tracker.calls.includes("assign acme/web#7"), "taking it assigns it to me");
  assert.ok(tracker.calls.some((c) => c.startsWith("comment acme/web#7")), "the issue gets a comment");
  assert.equal(rec.trackerStatus?.text, "Статус у GitLab: «In progress»");
  assert.ok(rec.summary?.includes("Added hello.txt"), "the summary falls back to the agent's message");
  orchestrator.stop();
});

test("a task deleted in the tracker is marked, not silently dropped", async () => {
  const { tracker, orchestrator } = setup("true");
  const t = issue(8, "Will be deleted");
  tracker.issues = [t];
  orchestrator.state.update(t.id, { task: t, status: "snoozed", snoozedUntil: Date.now() + 3600_000, repo: "web" });
  tracker.state.set(t.id, "deleted");
  await orchestrator.poll();
  const rec = orchestrator.state.get(t.id)!;
  assert.equal(rec.status, "cancelled");
  assert.equal(rec.gone, "deleted");
  orchestrator.stop();
});

test("a merged MR moves the task to its final status", async () => {
  const { tracker, orchestrator } = setup("true");
  const t = issue(9, "Merged");
  orchestrator.state.update(t.id, { task: t, status: "done", repo: "web", prUrl: "https://git.example/acme/web/-/merge_requests/1" });
  await orchestrator.refreshReview(t.id);
  assert.equal(orchestrator.state.get(t.id)!.reviewState, undefined, "still open");
  tracker.review = "merged";
  await orchestrator.refreshReview(t.id);
  const rec = orchestrator.state.get(t.id)!;
  assert.equal(rec.reviewState, "merged");
  assert.ok(tracker.calls.includes("merged acme/web#9"));
  assert.equal(rec.trackerStatus?.text, "Статус у GitLab: «Done»");
  orchestrator.stop();
});

test("«Взяти» assigns the task to me; a colleague's task is not offered", async () => {
  const { tracker, orchestrator } = setup("echo done");
  tracker.issues = [{ ...issue(10, "Free task"), assignees: [] }, { ...issue(11, "Colleague's task"), assignees: ["Colleague"] }];
  await orchestrator.poll();
  await orchestrator.drain();
  assert.equal(orchestrator.state.get("acme/web#11"), undefined, "someone else's task is left alone");
  assert.equal(orchestrator.state.get("acme/web#10")?.status, "done");
  assert.ok(tracker.calls.includes("assign acme/web#10"));
  orchestrator.stop();
});

test("a Jira task with its MR in GitLab gets CI, threads and merge from GitLab", async () => {
  // GitLab's API for one MR, as far as Mako reads it.
  let merged = false;
  const server = http.createServer((req, res) => {
    res.setHeader("content-type", "application/json");
    if (req.headers["private-token"] !== "glpat-test") return res.writeHead(401).end("{}");
    if (req.url?.endsWith("/merge") && req.method === "PUT") return (merged = true), res.end("{}");
    if (req.url?.includes("/discussions")) {
      return res.end(JSON.stringify([{ id: "d1", notes: [{ id: 5, system: false, resolvable: true, resolved: false, author: { name: "Ann" }, body: "Rename it" }] }]));
    }
    if (req.url?.includes("/pipelines/")) return res.end(JSON.stringify([{ id: 1, name: "unit", stage: "test", status: "failed", web_url: "u" }]));
    res.end(JSON.stringify({ state: merged ? "merged" : "opened", project_id: 1, web_url: "w", has_conflicts: false, detailed_merge_status: "mergeable", head_pipeline: { id: 9, status: "failed", web_url: "p" } }));
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const port = (server.address() as any).port;
  process.env.GITLAB_TOKEN = "glpat-test";
  try {
    const { tracker, orchestrator } = setup("true");
    // the task's tracker knows nothing about MRs, like Jira
    Object.defineProperty(tracker, "name", { value: "Jira" });
    (tracker as any).reviewDetails = undefined;
    const t = issue(12, "Jira task");
    orchestrator.state.update(t.id, { task: t, status: "done", repo: "web", prUrl: `http://127.0.0.1:${port}/acme/web/-/merge_requests/3` });
    const rec = await orchestrator.refreshReview(t.id);
    assert.equal(rec?.review?.pipeline?.status, "failed");
    assert.deepEqual(rec?.review?.pipeline?.failed.map((j) => j.name), ["unit"]);
    assert.equal(rec?.review?.comments[0]?.body, "Rename it");
    await orchestrator.mergeReview(t.id);
    assert.equal(orchestrator.state.get(t.id)?.reviewState, "merged");
    assert.ok(tracker.calls.includes("merged acme/web#12"), "the Jira issue moves on after the merge");
    orchestrator.stop();
  } finally {
    delete process.env.GITLAB_TOKEN;
    server.close();
  }
});
