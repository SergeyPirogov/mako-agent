import { test } from "node:test";
import assert from "node:assert/strict";
import { branchName, slugifyTitle, taskType } from "../src/git.ts";
import { renderMarkdown } from "../src/markdown.ts";
import { parseConfig, DEFAULT_BRANCH_PATTERN } from "../src/config.ts";
import { columnOf } from "../src/inbox.ts";
import type { Task } from "../src/types.ts";
import type { TaskRecord } from "../src/state.ts";

const task = (id: string, title: string, extra: Partial<Task> = {}): Task => ({
  id,
  slug: `web-${id.split("#").pop()}`,
  trackerId: "gitlab",
  project: "acme/web",
  title,
  description: "",
  url: "https://git.example/acme/web/-/issues/1",
  labels: [],
  components: [],
  comments: [],
  ...extra,
});

test("branch names follow the pattern", () => {
  assert.equal(branchName(DEFAULT_BRANCH_PATTERN, task("acme/web#17", "Розібрати результати нічного рану тестів")), "feat/17-rozibraty-rezultaty-nichnoho-ranu-testiv");
  assert.equal(branchName("feature/{id}-{title}", task("acme/web#42", "Add dark mode toggle")), "feature/42-add-dark-mode-toggle");
  assert.equal(branchName("{type}/{id}", task("acme/web#9", "Checkout shows error", { labels: ["bug"] })), "fix/9");
  assert.equal(branchName("{type}/{id}-{title}", task("PROJ-12", "Login: wrong colour?", { meta: "Type: Bug" })), "fix/PROJ-12-login-wrong-colour");
  assert.equal(branchName("agent/{slug}", task("acme/web#42", "x")), "agent/web-42");
  // a fresh attempt gets its own branch
  assert.equal(branchName("feat/{id}", task("acme/web#3", "x"), 2), "feat/3-try2");
  // nothing git refuses
  assert.equal(branchName("my branch/{id}..x~", task("acme/web#1", "x")), "my-branch/1-x");
});

test("titles become short git-safe slugs", () => {
  assert.equal(slugifyTitle("storefront-ui: PRODUCT_WITH_LONG_NAME sold out on stage, checkout shows error (@58929)"), "storefront-ui-product-with-long-name"); // at most 40 characters, whole words
  assert.equal(slugifyTitle("!!!"), "task");
  assert.equal(taskType(task("a#1", "Fix login")), "fix");
  assert.equal(taskType(task("a#1", "Add login")), "feat");
});

test("markdown renders GitLab descriptions safely", () => {
  const html = renderMarkdown(
    "## Кроки\n- [x] done\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n<script>alert(1)</script>\n\n[bad](javascript:alert(1)) [ok](https://x.dev) ![s](/uploads/abc/s.png)<!-- hidden -->",
    "https://git.example/acme/web/-/issues/1",
  );
  assert.match(html, /<h2>Кроки<\/h2>/);
  assert.match(html, /<table>/);
  assert.match(html, /type="checkbox"/);
  assert.doesNotMatch(html, /<script>/);
  assert.match(html, /&lt;script&gt;/);
  assert.doesNotMatch(html, /href="javascript/); // stays plain text, never a link
  assert.match(html, /href="https:\/\/x\.dev\/"/);
  assert.match(html, /src="https:\/\/git\.example\/acme\/web\/uploads\/abc\/s\.png"/);
  assert.doesNotMatch(html, /hidden/);
});

test("config: defaults, short project form, branch pattern per repo", () => {
  const cfg = parseConfig({
    branchPattern: "feature/{id}",
    executors: { bg: { type: "claude-headless" }, term: { type: "terminal" } },
    trackers: [
      {
        type: "gitlab",
        projects: {
          "acme/web": { repoPath: "/tmp/web" },
          "acme/api": { repos: { api: { repoPath: "/tmp/api", branchPattern: "fix/{id}" } } },
        },
      },
    ],
  });
  assert.equal(cfg.defaultExecutor, "term", "the Mako terminal is the default when none is chosen");
  assert.equal(cfg.trackers[0].projects["acme/web"].repos.web.branchPattern, "feature/{id}");
  assert.equal(cfg.trackers[0].projects["acme/api"].repos.api.branchPattern, "fix/{id}");
  assert.equal(cfg.trackers[0].projects["acme/web"].repos.web.baseBranch, "main");
  assert.throws(() => parseConfig({ trackers: [] }), /no trackers/);
});

test("board columns: an open MR is on review, not done", () => {
  const rec = (patch: Partial<TaskRecord>) => ({ id: "a#1", task: task("a#1", "x"), updatedAt: "", status: "done", ...patch }) as TaskRecord;
  assert.equal(columnOf(rec({ status: "asking" })), "new");
  assert.equal(columnOf(rec({ status: "running", prUrl: "u" })), "review", "an open MR is in review while the terminal is still open");
  assert.equal(columnOf(rec({ status: "running" })), "doing");
  assert.equal(columnOf(rec({ prUrl: "u" })), "review");
  assert.equal(columnOf(rec({ prUrl: "u", reviewState: "merged" })), "done");
  assert.equal(columnOf(rec({})), "done");
  assert.equal(columnOf(rec({ status: "cancelled" })), "failed");
});

test("GitLab «Взяти» adds me to the assignees instead of replacing them", async () => {
  const { createServer } = await import("node:http");
  const { GitLabTracker } = await import("../src/trackers/gitlab.ts");
  const puts: unknown[] = [];
  const server = createServer((req, res) => {
    let body = "";
    req.on("data", (d) => (body += d));
    req.on("end", () => {
      res.setHeader("content-type", "application/json");
      if (req.url === "/api/v4/user") return res.end(JSON.stringify({ id: 5, username: "me" }));
      if (req.method === "GET") return res.end(JSON.stringify({ assignees: [{ id: 7 }, { id: 5 }] }));
      puts.push(JSON.parse(body));
      res.end("{}");
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  try {
    const gl = new GitLabTracker(
      { type: "gitlab", baseUrl: `http://127.0.0.1:${(server.address() as any).port}`, projects: {} } as any,
      () => "token",
    );
    await gl.assignToMe(task("acme/web#3", "Shared"));
    assert.deepEqual(puts, [{ assignee_ids: [5, 7] }]);
  } finally {
    server.close();
  }
});

test("a project saved with an empty repos (all defaults) has its one repo", () => {
  const cfg = parseConfig({
    trackers: [{ type: "gitlab", baseUrl: "https://git.kbyte.app", projects: { "ecom/tests/moq": { repos: {} }, "ecom/web/front": {} } }],
  });
  const moq = cfg.trackers[0].projects["ecom/tests/moq"].repos;
  assert.deepEqual(Object.keys(moq), ["moq"]);
  assert.equal(moq.moq.cloneUrl, "git@git.kbyte.app:ecom/tests/moq.git");
  assert.match(moq.moq.repoPath, /mako\/projects\/moq$/);
  assert.deepEqual(Object.keys(cfg.trackers[0].projects["ecom/web/front"].repos), ["front"]);
});

test("the agent's menu is read off its screen: Claude's plan approval", async () => {
  const { parseChoice } = await import("../src/sessions.ts");
  const screen = [
    " 3. In Chrome open /admin/ui/ at ~800px height",
    "",
    " Claude has written up a plan and is ready to execute. Would you like to proceed?",
    "",
    " ❯ 1. Yes, and use auto mode",
    "   2. Yes, manually approve edits",
    "   3. Tell Claude what to change",
    "      shift+tab to approve with this feedback",
  ];
  const c = parseChoice(screen)!;
  assert.equal(c.question, "Claude has written up a plan and is ready to execute. Would you like to proceed?");
  assert.deepEqual(c.options.map((o) => [o.key, o.label, o.current]), [["1", "Yes, and use auto mode", true], ["2", "Yes, manually approve edits", false], ["3", "Tell Claude what to change", false]]);
  // a plan's own numbered steps are not a menu: no question right above them
  assert.equal(parseChoice(["Plan:", " 1. cd frontend && npm test", " 2. Start the dev server"]), undefined);
});
