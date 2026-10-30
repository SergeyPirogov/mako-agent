import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { parseConfig } from "../src/config.ts";
import { Orchestrator } from "../src/orchestrator.ts";
import { StateStore } from "../src/state.ts";
import { columnOf } from "../src/inbox.ts";
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
  async replyToThread(_url: string, thread: string, body: string, resolve: boolean) {
    this.calls.push(`reply ${thread}${resolve ? " resolved" : ""}: ${body.split("\n")[0]}`);
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

function setup(agentScript: string, extra: Record<string, unknown> = {}) {
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
    planFirst: false, // the plan has its own test
    ...extra,
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
  tracker.issues = [
    { ...issue(10, "Free task"), assignees: [] },
    { ...issue(11, "Colleague's task"), assignees: ["Colleague"] },
    { ...issue(12, "Shared task"), assignees: ["Colleague", "me"] },
  ];
  await orchestrator.poll();
  await orchestrator.drain();
  assert.equal(orchestrator.state.get("acme/web#11"), undefined, "someone else's task is left alone");
  assert.equal(orchestrator.state.get("acme/web#12")?.status, "done", "a task with me among two assignees is mine");
  assert.deepEqual(orchestrator.lastSearch?.othersIds, ["acme/web#11"], "the skipped one is named in the search line");
  assert.equal(orchestrator.state.get("acme/web#10")?.status, "done");
  assert.ok(tracker.calls.includes("assign acme/web#10"));
  orchestrator.stop();
});

test("skipOthersTasks: false offers tasks assigned to colleagues too", async () => {
  const { tracker, orchestrator } = setup("echo done");
  (orchestrator.cfg.trackers[0] as any).skipOthersTasks = false;
  tracker.issues = [{ ...issue(13, "Dev and QA on it"), assignees: ["Dev", "Qa"] }];
  await orchestrator.poll();
  await orchestrator.drain();
  assert.equal(orchestrator.state.get("acme/web#13")?.status, "done");
  assert.deepEqual(orchestrator.lastSearch?.othersIds, []);
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

test("the agent's answers to review threads are posted after the push, and fixed threads resolved", async () => {
  const { remote, tracker, orchestrator } = setup('echo "v1" > app.txt; echo "first version"');
  tracker.issues = [issue(13, "Review me")];
  await orchestrator.poll();
  await orchestrator.drain();
  const id = "acme/web#13";
  orchestrator.state.update(id, { prUrl: "https://git.example/acme/web/-/merge_requests/4" });
  const wt = orchestrator.workspaceFor(id)!.worktree;
  // what the agent does after «Виправити коментарі»: a fix, and its answers for Mako
  writeFileSync(path.join(wt, "app.txt"), "v2\n");
  execFileSync("mkdir", ["-p", path.join(wt, ".mako")]);
  writeFileSync(
    path.join(wt, ".mako/review-replies.json"),
    JSON.stringify([
      { thread: "d1", reply: "Renamed to TIMEOUT_MS in app.txt", resolved: true },
      { thread: "d2", reply: "Kept as is: the API needs it", resolved: false },
    ]),
  );
  assert.equal((await orchestrator.pendingChanges(id)).pending, true);
  const res = await orchestrator.publishNow(id);
  assert.equal(res.replied, 2);
  assert.ok(tracker.calls.includes("reply d1 resolved: 🦈 Renamed to TIMEOUT_MS in app.txt"));
  assert.ok(tracker.calls.includes("reply d2: 🦈 Kept as is: the API needs it"));
  const branch = orchestrator.state.get(id)!.branchName!;
  assert.equal(git(remote, "show", `${branch}:app.txt`), "v2", "the fix is pushed");
  assert.throws(() => git(remote, "show", `${branch}:.mako/review-replies.json`), "Mako's file is not committed");
  orchestrator.stop();
});

test("«+ Задача» with a plan first: plan, approve, then the agent works to it", async () => {
  const { remote, orchestrator } = setup('cat > /dev/null; echo "plan followed" > done.txt; echo "Done as planned"');
  // a stand-in for Claude in plan mode
  const bin = mkdtempSync(path.join(dir, "bin-"));
  writeFileSync(path.join(bin, "claude"), '#!/bin/sh\ncase "$*" in *plan*) cat > /dev/null; echo "## Як я це зроблю\\n1. Додати done.txt";; *) exit 1;; esac\n', { mode: 0o755 });
  const savedPath = process.env.PATH;
  process.env.PATH = `${bin}:${savedPath}`;
  try {
    const id = orchestrator.createTask({ title: "Write done.txt", description: "", repo: "web", executor: "agent", planFirst: true });
    assert.equal(id, "mako#1");
    await orchestrator.drain();
    let rec = orchestrator.state.get(id)!;
    assert.equal(rec.status, "planned", rec.error);
    assert.match(rec.plan!, /Додати done\.txt/);
    assert.equal(orchestrator.wip(), 1, "a task waiting for its plan holds a WIP slot");

    assert.ok(orchestrator.answerPlan(id, "approve"));
    await orchestrator.drain();
    rec = orchestrator.state.get(id)!;
    assert.equal(rec.status, "done", rec.error);
    assert.match(readFileSync(orchestrator.state.logPath(rec.task).replace(/\.jsonl$/, ".prompt.md"), "utf8"), /## Approved plan[\s\S]*Додати done\.txt/);
    assert.equal(git(remote, "show", `${rec.branchName}:done.txt`), "plan followed");
  } finally {
    process.env.PATH = savedPath;
    orchestrator.stop();
  }
});

test("templates and the repo's notes go into the prompt; a finished task updates the notes", async () => {
  const { orchestrator } = setup('echo "ran pnpm test:unit, it needs NODE_ENV=test"', {
    templates: [{ name: "Нічний ран", labels: ["nightly"], instructions: "Classify every failure: flaky, product bug or test bug." }],
  });
  const stateDir = path.dirname(orchestrator.state.logPath(issue(1, "x")).replace(/logs\/.*$/, "x"));
  mkdirSync(path.join(stateDir, "memory"), { recursive: true });
  writeFileSync(path.join(stateDir, "memory", "web.md"), "- Tests: `pnpm test:unit`\n");
  const bin = mkdtempSync(path.join(dir, "bin-"));
  // Claude for the notes: answers the memory prompt, nothing else
  writeFileSync(path.join(bin, "claude"), '#!/bin/sh\nin=$(cat)\ncase "$in" in *"You keep the notes"*) echo "- Tests: \\`pnpm test:unit\\` with NODE_ENV=test";; *) exit 1;; esac\n', { mode: 0o755 });
  const savedPath = process.env.PATH;
  process.env.PATH = `${bin}:${savedPath}`;
  try {
    const t = { ...issue(14, "Triage the night"), labels: ["nightly"] };
    (orchestrator.trackers.get("gitlab") as FakeTracker).issues = [t];
    await orchestrator.poll();
    await orchestrator.drain();
    const rec = orchestrator.state.get(t.id)!;
    assert.equal(rec.status, "done", rec.error);
    const prompt = readFileSync(orchestrator.state.logPath(rec.task).replace(/\.jsonl$/, ".prompt.md"), "utf8");
    assert.match(prompt, /«Нічний ран»[\s\S]*Classify every failure/);
    assert.match(prompt, /What earlier tasks learned[\s\S]*pnpm test:unit/);
    // the notes are updated in the background
    for (let i = 0; i < 50 && !readFileSync(path.join(stateDir, "memory", "web.md"), "utf8").includes("NODE_ENV"); i++) await new Promise((r) => setTimeout(r, 100));
    assert.match(readFileSync(path.join(stateDir, "memory", "web.md"), "utf8"), /NODE_ENV=test/);
  } finally {
    process.env.PATH = savedPath;
    orchestrator.stop();
  }
});

test("MR autopilot: hands the agent failed CI and new threads once each, stops at the round limit, merges a ready MR", async () => {
  // each run leaves a change, so every round pushes something
  const { orchestrator, tracker } = setup("date +%s%N >> work.txt", { autopilot: { enabled: true, maxRounds: 2, merge: true } });
  try {
    const t = issue(21, "Fix the cart");
    tracker.issues = [t];
    await orchestrator.poll();
    await orchestrator.drain();
    assert.equal(orchestrator.state.get(t.id)!.status, "done");
    const mr = "https://git.example/acme/web/-/merge_requests/9";
    orchestrator.state.update(t.id, { prUrl: mr });
    let details: ReviewInfo;
    let merged = false;
    tracker.reviewDetails = async () => ({ ...details, checkedAt: Date.now() });
    (tracker as any).mergeReview = async () => {
      merged = true;
      details = { ...details, state: "merged" };
    };
    const lastPrompt = () => readFileSync(orchestrator.state.logPath(orchestrator.state.get(t.id)!.task).replace(/\.jsonl$/, ".prompt.md"), "utf8");
    const look = async () => {
      await orchestrator.refreshReview(t.id);
      await orchestrator.drain();
      return orchestrator.state.get(t.id)!;
    };

    // CI failed: round 1, the agent gets the CI request
    details = { state: "open", conflicts: false, mergeable: false, comments: [], checkedAt: 0, pipeline: { status: "failed", url: `${mr}/pipelines/1`, failed: [{ name: "e2e" }] } };
    let rec = await look();
    assert.equal(rec.status, "done", rec.error);
    assert.equal(rec.autopilotRounds, 1);
    assert.match(lastPrompt(), /The CI pipeline of MR .* failed/);

    // the same failed pipeline again: not handed twice
    rec = await look();
    assert.equal(rec.autopilotRounds, 1);

    // a reviewer's thread: round 2
    details = { state: "open", conflicts: false, mergeable: false, comments: [{ author: "Oleh", body: "Why 20?", replies: 0, thread: "t1" }], checkedAt: 0, pipeline: { status: "success", failed: [] } };
    rec = await look();
    assert.equal(rec.autopilotRounds, 2);
    assert.match(lastPrompt(), /\[thread t1\] Oleh/);

    // CI fails once more: the limit is reached, the developer is called, nothing runs
    details = { ...details, comments: [], pipeline: { status: "failed", url: `${mr}/pipelines/3`, failed: [{ name: "lint" }] } };
    rec = await look();
    assert.equal(rec.autopilotRounds, 2);
    assert.match(rec.autopilotNote!, /зупинився після 2 раундів/);
    assert.ok(notices.some((n) => n.includes("автопілот зупинився")));

    // turned off and on from the card: fresh rounds, the failed CI goes to the agent
    orchestrator.setAutopilot(t.id, false);
    assert.equal((await look()).autopilotRounds, 2, "off: nothing happens");
    orchestrator.setAutopilot(t.id, true);
    rec = await look();
    assert.equal(rec.autopilotRounds, 1);
    assert.match(lastPrompt(), /pipelines\/3/);

    // all green, approved: merged without a click
    details = { state: "open", conflicts: false, mergeable: true, comments: [], checkedAt: 0, pipeline: { status: "success", failed: [] } };
    await look();
    assert.ok(merged, "the ready MR is merged");
    assert.equal(orchestrator.state.get(t.id)!.reviewState, "merged");
  } finally {
    orchestrator.stop();
  }
});

test("MR autopilot is off by default: a failed CI only notifies", async () => {
  const { orchestrator, tracker } = setup("echo hi > hi.txt");
  try {
    const t = issue(22, "Quiet");
    tracker.issues = [t];
    await orchestrator.poll();
    await orchestrator.drain();
    orchestrator.state.update(t.id, { prUrl: "https://git.example/acme/web/-/merge_requests/10" });
    tracker.reviewDetails = async () => ({ state: "open", conflicts: false, mergeable: false, comments: [], checkedAt: Date.now(), pipeline: { status: "failed", failed: [{ name: "e2e" }] } });
    await orchestrator.refreshReview(t.id);
    await orchestrator.drain();
    const rec = orchestrator.state.get(t.id)!;
    assert.equal(rec.autopilotRounds, undefined);
    assert.equal(rec.status, "done");
  } finally {
    orchestrator.stop();
  }
});

test("an MR closed without merging sends the task to «Не виконано»", async () => {
  const { tracker, orchestrator } = setup("true");
  const t = issue(23, "Rejected");
  orchestrator.state.update(t.id, { task: t, status: "done", repo: "web", prUrl: "https://git.example/acme/web/-/merge_requests/2" });
  tracker.review = "closed";
  await orchestrator.refreshReview(t.id);
  const rec = orchestrator.state.get(t.id)!;
  assert.equal(rec.reviewState, "closed");
  assert.equal(rec.status, "failed");
  assert.equal(rec.error, "MR закрито без мерджу");
  assert.equal(columnOf(rec), "failed");
  assert.ok(!tracker.calls.some((c) => c.startsWith("merged")), "the issue does not move to Done");
  orchestrator.stop();
});

test("a repo without a path is cloned into the projects folder, its base branch found by itself", async () => {
  const base = mkdtempSync(path.join(dir, "clone-"));
  const remote = path.join(base, "remote.git");
  const seed = path.join(base, "seed");
  // the remote's main branch is «trunk»: nothing in the config says so
  execFileSync("git", ["init", "-q", "--bare", "-b", "trunk", remote]);
  execFileSync("git", ["clone", "-q", remote, seed]);
  for (const a of [["config", "user.email", "t@t"], ["config", "user.name", "T"], ["commit", "-q", "--allow-empty", "-m", "init"], ["push", "-q", "origin", "trunk"]]) git(seed, ...a);
  const agent = path.join(base, "agent.sh");
  writeFileSync(agent, "#!/bin/sh\necho cloned-and-done > done.txt\n", { mode: 0o755 });
  const cfg = parseConfig({
    stateDir: path.join(base, "state"),
    projectsDir: path.join(base, "projects"),
    planFirst: false,
    notifications: { enabled: false, autoTake: true, inboxPort: 0 },
    executors: { agent: { type: "command", command: agent } },
    defaultExecutor: "agent",
    trackers: [{ type: "gitlab", baseUrl: "https://git.example", projects: { "acme/web": { cloneUrl: remote, pullRequest: "none", setup: "none" } } }],
  });
  const repo = cfg.trackers[0].projects["acme/web"].repos.web;
  assert.equal(repo.repoPath, path.join(base, "projects", "web"));
  assert.equal(parseConfig({ ...structuredClone({ trackers: [{ type: "gitlab", baseUrl: "https://git.example", projects: { "acme/web": {} } }] }) }).trackers[0].projects["acme/web"].repos.web.cloneUrl,
    "git@git.example:acme/web.git", "the GitLab project over SSH by default");
  const tracker = new FakeTracker(cfg.trackers[0].projects);
  const orchestrator = new Orchestrator(cfg, [tracker], new StateStore(cfg.stateDir));
  try {
    const t = issue(24, "First task in a fresh clone");
    // git config for commits in the new clone
    process.env.GIT_AUTHOR_NAME = process.env.GIT_COMMITTER_NAME = "T";
    process.env.GIT_AUTHOR_EMAIL = process.env.GIT_COMMITTER_EMAIL = "t@t";
    tracker.issues = [t];
    await orchestrator.poll();
    await orchestrator.drain();
    const rec = orchestrator.state.get(t.id)!;
    assert.equal(rec.status, "done", rec.error);
    assert.ok(existsSync(path.join(repo.repoPath, ".git")), "cloned");
    assert.equal(repo.baseBranch, "trunk", "the remote's default branch");
    assert.equal(git(remote, "show", `${rec.branchName}:done.txt`), "cloned-and-done");
  } finally {
    for (const k of ["GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"]) delete process.env[k];
    orchestrator.stop();
  }
});

test("a repo that cannot be cloned fails with the reason, not a crash", async () => {
  const base = mkdtempSync(path.join(dir, "noclone-"));
  const cfg = parseConfig({
    stateDir: path.join(base, "state"),
    projectsDir: path.join(base, "projects"),
    planFirst: false,
    notifications: { enabled: false, autoTake: true, inboxPort: 0 },
    executors: { agent: { type: "command", command: "true" } },
    defaultExecutor: "agent",
    trackers: [{ type: "gitlab", projects: { "acme/web": { cloneUrl: path.join(base, "missing.git"), setup: "none" } } }],
  });
  const tracker = new FakeTracker(cfg.trackers[0].projects);
  const orchestrator = new Orchestrator(cfg, [tracker], new StateStore(cfg.stateDir));
  tracker.issues = [issue(25, "Nowhere to clone from")];
  await orchestrator.poll();
  await orchestrator.drain();
  const rec = orchestrator.state.get("acme/web#25")!;
  assert.equal(rec.status, "failed");
  assert.match(rec.error!, /Не вдалось склонувати/);
  assert.ok(!existsSync(path.join(base, "projects", "web")), "no half-made folder is left");
  orchestrator.stop();
});
