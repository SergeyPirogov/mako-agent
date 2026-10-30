import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import os from "node:os";
import path from "node:path";
import { parseConfig } from "../src/config.ts";
import { createTracker } from "../src/trackers/index.ts";
import { JiraTracker } from "../src/trackers/jira.ts";
import { Orchestrator } from "../src/orchestrator.ts";
import { StateStore } from "../src/state.ts";
import { setNotificationHost } from "../src/notify.ts";
import type { Task } from "../src/types.ts";

// Jira Cloud's REST API, as far as Mako uses it: search, issue, transitions, comments, assignee, statuses.
const AUTH = `Basic ${Buffer.from("me@acme.dev:jira-token").toString("base64")}`;
const adf = (text: string) => ({ type: "doc", version: 1, content: [{ type: "paragraph", content: [{ type: "text", text }] }] });
type Issue = { key: string; summary: string; status: string; assignee?: string; labels: string[]; deleted?: boolean };
const jira = {
  issues: [] as Issue[],
  calls: [] as string[],
  comments: [] as string[],
  workflow: ["To Do", "In Progress", "In Review", "Done"],
};
let server: http.Server;
let baseUrl = "";
let dir = "";

before(async () => {
  server = http.createServer((req, res) => {
    let body = "";
    req.on("data", (d) => (body += d));
    req.on("end", () => {
      res.setHeader("content-type", "application/json");
      if (req.headers.authorization !== AUTH) return res.writeHead(401).end('{"errorMessages":["auth"]}');
      const url = new URL(req.url!, "http://x");
      const issueOf = (key: string) => jira.issues.find((i) => i.key === key && !i.deleted);
      const send = (data: unknown) => res.end(JSON.stringify(data));
      if (req.method === "POST" && url.pathname === "/rest/api/3/search/jql") {
        jira.calls.push(`search ${JSON.parse(body).jql}`);
        return send({
          issues: jira.issues
            .filter((i) => !i.deleted && i.status === "To Do")
            .map((i) => ({
              key: i.key,
              fields: {
                summary: i.summary,
                description: adf(`Steps for ${i.key}`),
                issuetype: { name: "Bug" },
                priority: { name: "High" },
                project: { key: "PROJ" },
                labels: i.labels,
                components: [],
                comment: { comments: [{ author: { displayName: "Ann" }, body: adf("Repro on stage only") }] },
                assignee: i.assignee ? { accountId: i.assignee } : null,
                attachment: [{ filename: "spec.pdf", content: `${baseUrl}/rest/api/3/attachment/content/10` }],
              },
            })),
        });
      }
      if (url.pathname === "/rest/api/3/myself") return send({ accountId: "acc-me" });
      if (url.pathname === "/rest/api/3/attachment/content/10") return res.end("%PDF");
      if (url.pathname === "/rest/api/3/project/search") return send({ values: [{ key: "PROJ", name: "Checkout" }, { key: "OPS", name: "Operations" }] });
      if (url.pathname === "/rest/api/3/project/PROJ/statuses") return send([{ name: "Bug", statuses: jira.workflow.map((name) => ({ name })) }]);
      const m = url.pathname.match(/^\/rest\/api\/3\/issue\/([A-Z]+-\d+)(\/\w+)?$/);
      const issue = m && issueOf(m[1]);
      if (m && !issue) return res.writeHead(404).end('{"errorMessages":["Issue does not exist"]}');
      if (issue && !m![2] && req.method === "GET") {
        return send({ fields: { status: { name: issue.status, statusCategory: { key: issue.status === "Done" ? "done" : "indeterminate" } } } });
      }
      if (issue && m![2] === "/transitions" && req.method === "GET") {
        return send({ transitions: jira.workflow.filter((s) => s !== issue.status).map((s, i) => ({ id: String(i + 1), name: `Move to ${s}`, to: { name: s } })) });
      }
      if (issue && m![2] === "/transitions" && req.method === "POST") {
        const id = JSON.parse(body).transition.id;
        const to = jira.workflow.filter((s) => s !== issue.status)[Number(id) - 1];
        jira.calls.push(`${issue.key} → ${to}`);
        issue.status = to;
        return res.writeHead(204).end();
      }
      if (issue && m![2] === "/comment") {
        const doc = JSON.parse(body).body;
        jira.comments.push(JSON.stringify(doc));
        return res.writeHead(201).end("{}");
      }
      if (issue && m![2] === "/assignee") {
        issue.assignee = JSON.parse(body).accountId;
        jira.calls.push(`${issue.key} assigned to ${issue.assignee}`);
        return res.writeHead(204).end();
      }
      res.writeHead(404).end("{}");
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  baseUrl = `http://127.0.0.1:${(server.address() as any).port}`;
  dir = mkdtempSync(path.join(os.tmpdir(), "mako-jira-"));
  process.env.PATH = "/usr/bin:/bin";
  process.env.HOME = dir;
  setNotificationHost({ ask: async () => "take", inform: () => {} });
});
after(() => {
  server.close();
  rmSync(dir, { recursive: true, force: true });
});

const env = (name: string) => ({ JIRA_EMAIL: "me@acme.dev", JIRA_API_TOKEN: "jira-token" })[name as "JIRA_EMAIL"] ?? (() => { throw new Error(`Missing ${name}`); })();
const jiraCfg = (extra: object = {}) => ({
  type: "jira" as const,
  baseUrl,
  jql: 'project = PROJ AND assignee = currentUser() AND statusCategory = "To Do"',
  transitions: { onStart: "In Progress", onSuccess: "In Review" },
  projects: { PROJ: { repos: { web: { repoPath: "/tmp/none", name: "web" } } } } as any,
  ...extra,
});

test("Jira: issues come with text from ADF, comments, labels and who they are assigned to", async () => {
  jira.issues = [{ key: "PROJ-1", summary: "Checkout fails", status: "To Do", labels: ["claude-agent"], assignee: "acc-me" }];
  const tracker = new JiraTracker(jiraCfg(), env);
  const [task] = await tracker.fetchTasks();
  assert.equal(task.id, "PROJ-1");
  assert.equal(task.description.trim(), "Steps for PROJ-1");
  assert.deepEqual(task.comments, [{ author: "Ann", body: "Repro on stage only" }]);
  assert.equal(task.meta, "Type: Bug | Priority: High");
  assert.deepEqual(task.assignees, ["acc-me"]);
  assert.deepEqual(task.attachments, [{ name: "spec.pdf", url: `${baseUrl}/rest/api/3/attachment/content/10`, where: "вкладення задачі" }]);
  assert.equal((await tracker.downloadAttachment(task.attachments![0], 1000)).toString(), "%PDF");
  assert.equal(task.url, `${baseUrl}/browse/PROJ-1`);
  assert.deepEqual(await tracker.listStatuses(), ["To Do", "In Progress", "In Review", "Done"]);
  assert.equal(await tracker.whoAmI(), "acc-me");
  assert.deepEqual(await tracker.listProjects(), [{ key: "PROJ", name: "PROJ — Checkout" }, { key: "OPS", name: "OPS — Operations" }]);
});

test("Jira: a wrong token says what to fix; a missing transition is reported, not guessed", async () => {
  const bad = new JiraTracker(jiraCfg(), (n) => (n === "JIRA_API_TOKEN" ? "wrong" : env(n)));
  await assert.rejects(bad.fetchTasks(), /JIRA_EMAIL і API-токен/);
  jira.issues = [{ key: "PROJ-2", summary: "x", status: "To Do", labels: [] }];
  const tracker = new JiraTracker(jiraCfg({ transitions: { onStart: "Doing" } }), env);
  const task = { id: "PROJ-2" } as Task;
  await assert.rejects(tracker.onStart(task), /немає переходу в «Doing»/);
  assert.equal(await tracker.issueState(task), "open");
  jira.issues[0].deleted = true;
  assert.equal(await tracker.issueState(task), "deleted");
});

test("Jira: the whole loop — take, In Progress, agent, push, comment, In Review, then Done after the merge", async () => {
  const base = mkdtempSync(path.join(dir, "case-"));
  const remote = path.join(base, "remote.git");
  const repo = path.join(base, "web");
  const git = (cwd: string, ...args: string[]) => execFileSync("git", args, { cwd, encoding: "utf8" }).trim();
  execFileSync("git", ["init", "-q", "--bare", "-b", "main", remote]);
  execFileSync("git", ["clone", "-q", remote, repo]);
  for (const a of [["config", "user.email", "t@t"], ["config", "user.name", "T"], ["commit", "-q", "--allow-empty", "-m", "init"], ["push", "-q", "origin", "main"]]) git(repo, ...a);
  const agent = path.join(base, "agent.sh");
  writeFileSync(agent, '#!/bin/sh\necho fixed > fix.txt\necho "Fixed the checkout"\n', { mode: 0o755 });
  const cfg = parseConfig({
    stateDir: path.join(base, "state"),
    planFirst: false,
    notifications: { enabled: false, autoTake: true, inboxPort: 0 },
    executors: { agent: { type: "command", command: agent } },
    defaultExecutor: "agent",
    trackers: [{ ...jiraCfg(), projects: { PROJ: { repoPath: repo, pullRequest: "none", setup: "none" } } }],
  });
  jira.issues = [
    { key: "PROJ-7", summary: "Checkout fails on sold-out items", status: "To Do", labels: [] },
    { key: "PROJ-8", summary: "Someone else's", status: "To Do", labels: [], assignee: "acc-colleague" },
  ];
  jira.calls = [];
  jira.comments = [];
  const tracker = createTracker(cfg.trackers[0], env);
  const o = new Orchestrator(cfg, [tracker], new StateStore(cfg.stateDir));
  await o.poll();
  await o.drain();

  const rec = o.state.get("PROJ-7")!;
  assert.equal(rec.status, "done", rec.error);
  assert.equal(o.state.get("PROJ-8"), undefined, "a colleague's issue is not offered");
  assert.equal(rec.branchName, "fix/PROJ-7-checkout-fails-on-sold-out-items", "a Bug gets a fix/ branch");
  assert.equal(git(remote, "show", `${rec.branchName}:fix.txt`), "fixed");
  assert.ok(jira.calls.includes("PROJ-7 assigned to acc-me"));
  assert.ok(jira.calls.includes("PROJ-7 → In Progress"));
  assert.ok(jira.calls.includes("PROJ-7 → In Review"));
  assert.ok(jira.comments.some((c) => c.includes("Fixed the checkout")), "the agent's summary is in a Jira comment");
  const prompt = readFileSync(o.state.logPath(rec.task).replace(/\.jsonl$/, ".prompt.md"), "utf8");
  assert.match(prompt, /Steps for PROJ-7/);
  assert.match(prompt, /Repro on stage only/);

  // merged in GitLab: the Jira issue goes to Done without any setting
  o.state.update("PROJ-7", { prUrl: "https://git.example/acme/web/-/merge_requests/3" });
  (o as any).reviews.reviewHost = () => ({ reviewDetails: async () => ({ state: "merged", conflicts: false, mergeable: false, comments: [], checkedAt: 0 }) });
  await o.refreshReview("PROJ-7");
  assert.ok(jira.calls.includes("PROJ-7 → Done"));
  assert.equal(o.state.get("PROJ-7")!.reviewState, "merged");
  o.stop();
});
