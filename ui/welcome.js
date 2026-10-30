// Setup wizard: from a fresh install to the first task in five steps. Writes the same config as the settings page.
(() => {
  "use strict";
  const TOKEN = document.querySelector('meta[name="mako-token"]').content;
  const root = document.getElementById("welcome");

  const STEPS = ["Перевірка", "Трекер", "Задачі", "Код", "Агент"];
  const NAMES = { github: "GitHub", gitlab: "GitLab", jira: "Jira" };
  const SECRET = { github: "GITHUB_TOKEN", gitlab: "GITLAB_TOKEN", jira: "JIRA_API_TOKEN" };

  let step = 0;
  let busy = false;
  let error = "";
  let settings = null; // GET /api/settings
  let doc = null; // GET /api/doctor
  const d = {
    link: "", type: "", baseUrl: "", project: "", token: "", email: "",
    mode: "mine", label: "claude-agent",
    test: null, // { count, sample, filter } after a successful check
    found: null, repoPath: "", baseBranch: "main", check: null,
    executor: "terminal",
  };

  // Closing the window destroys the page: keep the step and answers so reopening picks up where it was left.
  // The token stays out: localStorage is a plain file on disk.
  const DRAFT = "mako.welcome";
  function keepDraft() {
    try {
      if (step >= STEPS.length) localStorage.removeItem(DRAFT);
      else localStorage.setItem(DRAFT, JSON.stringify({ step, d: { ...d, token: "" } }));
    } catch {}
  }
  try {
    const saved = JSON.parse(localStorage.getItem(DRAFT) || "null");
    if (saved?.d) {
      Object.assign(d, saved.d);
      step = Math.min(Math.max(Number(saved.step) || 0, 0), STEPS.length - 1);
    }
  } catch {}

  async function api(method, url, body) {
    const res = await fetch(url, {
      method,
      headers: { "Content-Type": "application/json", "X-Mako-Token": TOKEN },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
    return data;
  }

  const h = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

  // ---------- what the colleague pasted ----------

  /** A link to a project or an issue tells the tracker, its address and the project in one go. */
  function parseLink(text) {
    const s = text.trim();
    if (!s) return {};
    if (!/^[a-z]+:\/\//i.test(s)) {
      if (/^[A-Z][A-Z0-9_]+$/.test(s)) return { type: "jira", project: s };
      if (/^[\w.-]+(\/[\w.-]+)+$/.test(s)) return { project: s.replace(/\.git$/, "") };
      return {};
    }
    let u;
    try {
      u = new URL(s);
    } catch {
      return {};
    }
    const parts = u.pathname.split("/").filter(Boolean);
    if (u.hostname === "github.com") {
      return { type: "github", baseUrl: "", project: parts.slice(0, 2).join("/").replace(/\.git$/, "") };
    }
    const issue = u.pathname.match(/\/browse\/([A-Z][A-Z0-9_]+)-\d+/) || u.pathname.match(/\/projects\/([A-Z][A-Z0-9_]+)/);
    if (issue || u.hostname.endsWith(".atlassian.net")) {
      return { type: "jira", baseUrl: u.origin, project: issue ? issue[1] : "" };
    }
    const dash = parts.indexOf("-");
    return { type: "gitlab", baseUrl: u.origin, project: (dash >= 0 ? parts.slice(0, dash) : parts).join("/").replace(/\.git$/, "") };
  }

  function tokenLink() {
    if (d.type === "github") return "https://github.com/settings/tokens/new?description=Mako&scopes=repo";
    if (d.type === "gitlab") return `${d.baseUrl || "https://gitlab.com"}/-/user_settings/personal_access_tokens?name=Mako&scopes=api`;
    return "https://id.atlassian.com/manage-profile/security/api-tokens";
  }

  /** What Mako clones from when the project has no local clone yet (SSH, as developers usually clone). */
  function cloneUrl() {
    if (!d.project.includes("/")) return "";
    try {
      if (d.type === "github") return `git@github.com:${d.project}.git`;
      if (d.type === "gitlab") return `git@${new URL(d.baseUrl || "https://gitlab.com").hostname}:${d.project}.git`;
    } catch {}
    return "";
  }

  const defaultFolder = () => `~/mako/projects/${d.project.split("/").pop() || "repo"}`;

  const savedSecret = (name) => Boolean(settings?.secrets?.[name]?.set);
  const ghLogin = () => d.type === "github" && doc?.ghLoggedIn;

  function secrets() {
    const s = {};
    if (d.token.trim()) s[SECRET[d.type]] = d.token.trim();
    if (d.type === "jira" && d.email.trim()) s.JIRA_EMAIL = d.email.trim();
    return s;
  }

  /** The tracker entry for mako.config.json; without a chosen folder yet, for the check only. */
  function tracker(repo) {
    const projects = { [d.project]: repo ?? { repoPath: "~" } };
    if (d.type === "jira") {
      const who = d.mode === "mine" ? "assignee = currentUser()" : `labels = "${d.label.replace(/"/g, "")}"`;
      return {
        type: "jira",
        baseUrl: d.baseUrl,
        jql: `project = ${d.project} AND ${who} AND statusCategory = "To Do" ORDER BY priority DESC`,
        projects,
      };
    }
    const t = { type: d.type };
    if (d.type === "gitlab") t.baseUrl = d.baseUrl || "https://gitlab.com";
    if (d.mode === "mine") {
      t.assignee = "me";
      t.labels = { trigger: null };
    } else {
      t.labels = { trigger: d.label.trim() || "claude-agent" };
    }
    t.projects = projects;
    return t;
  }

  // ---------- rendering ----------

  function render() {
    keepDraft();
    const steps = STEPS.map(
      (name, i) => `<li class="${i < step ? "done" : i === step ? "on" : ""}"><span>${i < step ? "✓" : i + 1}</span>${name}</li>`,
    ).join("");
    const body = step >= STEPS.length ? viewDone() : [viewDoctor, viewTracker, viewTasks, viewCode, viewAgent][step]();
    root.innerHTML = `
      <h1>Початок роботи з Mako</h1>
      <p class="muted">Кілька кроків, і Mako почне приносити вам задачі. Усе це потім можна змінити в Налаштуваннях.</p>
      ${step < STEPS.length ? `<ol class="wz-steps">${steps}</ol>` : ""}
      <div class="card wz-card">${body}${error ? `<div class="notice err wz-err">${h(error)}</div>` : ""}</div>`;
    const first = root.querySelector("[autofocus]");
    if (first && document.activeElement === document.body) first.focus();
  }

  const nav = (next, opts = {}) => `<div class="wz-nav">
      ${step > 0 && step < STEPS.length ? `<button type="button" data-act="back">Назад</button>` : "<span></span>"}
      <button type="button" class="primary" data-act="next" ${opts.disabled || busy ? "disabled" : ""}>${next}</button></div>`;

  function viewDoctor() {
    if (!doc) return `<p class="muted">Перевіряю, що встановлено…</p>`;
    const row = (ok, name, okText, missing) =>
      `<li class="${ok ? "ok" : "miss"}"><span class="dot">${ok ? "✓" : "!"}</span><div><strong>${name}</strong>
        <div class="small muted">${ok ? h(okText) : missing}</div></div></li>`;
    return `<h2>Що потрібно на цьому компʼютері</h2>
      <ul class="wz-checks">
        ${row(doc.git, "git", doc.git, "Не знайдено. На Mac: <code>xcode-select --install</code>")}
        ${row(doc.claude, "Claude Code", doc.claude, "Не знайдено. Встановіть: <code>npm install -g @anthropic-ai/claude-code</code>, потім запустіть <code>claude</code> один раз, щоб увійти")}
        ${doc.gh ? row(true, "GitHub CLI", doc.ghLoggedIn ? "увійшли, токен для GitHub не знадобиться" : "встановлено") : ""}
      </ul>
      ${!doc.git || !doc.claude ? `<p class="small muted">Можна продовжити й встановити пізніше. <button type="button" class="link" data-act="recheck">Перевірити ще раз</button></p>` : ""}
      ${nav("Далі")}`;
  }

  function viewTracker() {
    const needUrl = d.type === "gitlab" || d.type === "jira";
    const secretName = SECRET[d.type];
    const tokenKnown = d.type && (savedSecret(secretName) || ghLogin());
    return `<h2>Де живуть ваші задачі?</h2>
      <label class="f">Посилання на проєкт або будь-яку задачу в ньому
        <input type="text" data-k="link" value="${h(d.link)}" placeholder="https://gitlab.com/group/project/-/issues/12" autofocus>
        <span class="hint">Mako сам зрозуміє, що це: GitHub, GitLab чи Jira.</span></label>
      <div class="wz-types">${Object.entries(NAMES)
        .map(([k, n]) => `<button type="button" class="${d.type === k ? "on" : ""}" data-type="${k}">${n}</button>`)
        .join("")}</div>
      ${d.type ? `<div class="grid">
        ${needUrl ? `<label class="f">Адреса ${NAMES[d.type]}<input type="text" data-k="baseUrl" value="${h(d.baseUrl)}" placeholder="${d.type === "jira" ? "https://company.atlassian.net" : "https://gitlab.com"}"></label>` : ""}
        <label class="f">${d.type === "jira" ? "Ключ проєкту" : "Проєкт"}<input type="text" data-k="project" value="${h(d.project)}" placeholder="${d.type === "jira" ? "PROJ" : d.type === "github" ? "owner/repo" : "group/project"}"></label>
        ${d.type === "jira" ? `<label class="f">Ваш email в Atlassian<input type="text" data-k="email" value="${h(d.email)}" placeholder="${savedSecret("JIRA_EMAIL") ? "збережено" : "me@company.com"}"></label>` : ""}
        <label class="f wide">Токен ${tokenKnown ? `<span class="badge ok">${ghLogin() && !savedSecret(secretName) ? "візьму з GitHub CLI" : "уже збережено"}</span>` : ""}
          <input type="password" data-k="token" value="${h(d.token)}" placeholder="${tokenKnown ? "можна не вводити" : "вставте токен"}" autocomplete="off">
          <span class="hint"><a href="${h(tokenLink())}" target="_blank" rel="noreferrer">Створити токен ↗</a> ·
            ${d.type === "gitlab" ? "потрібен доступ <code>api</code>" : d.type === "github" ? "потрібен доступ <code>repo</code>" : "API token вашого акаунта"}.
            Токен зберігається лише на цьому компʼютері.</span></label>
      </div>` : ""}
      ${nav("Далі", { disabled: !d.type || !d.project || (needUrl && !d.baseUrl) })}`;
  }

  function viewTasks() {
    const t = d.test;
    return `<h2>Які задачі брати?</h2>
      <div class="wz-options">
        <label class="wz-opt ${d.mode === "mine" ? "on" : ""}"><input type="radio" name="mode" value="mine" ${d.mode === "mine" ? "checked" : ""}>
          <div><strong>Призначені на мене</strong><div class="small muted">Mako пропонує все, що в ${h(d.project)} призначено на вас.</div></div></label>
        <label class="wz-opt ${d.mode === "label" ? "on" : ""}"><input type="radio" name="mode" value="label" ${d.mode === "label" ? "checked" : ""}>
          <div><strong>Лише з міткою</strong><div class="small muted">Тільки задачі з міткою, яку ви ставите вручну, на кого б вони не були.</div>
          ${d.mode === "label" ? `<input type="text" data-k="label" value="${h(d.label)}" class="wz-label">` : ""}</div></label>
      </div>
      <div class="adds"><button type="button" data-act="test" ${busy ? "disabled" : ""}>${busy ? "Перевіряю…" : "Перевірити"}</button>
        ${t ? `<span class="${t.count ? "ok-text" : "warn"}">${t.count ? `✓ Знайдено задач: ${t.count}` : "Підключення працює, але задач зараз немає"}</span>` : ""}</div>
      ${t?.sample?.length ? `<ul class="wz-sample">${t.sample.map((s) => `<li><span class="muted">${h(s.id)}</span> ${h(s.title)}</li>`).join("")}</ul>` : ""}
      ${t && !t.count && t.filter ? `<p class="small muted">Фільтр: ${h(t.filter)}</p>` : ""}
      ${t?.statuses?.length ? `<p class="small muted">Статуси задач у ${NAMES[d.type]}: ${t.statuses.map(h).join(", ")}. Mako перемикатиме їх сам: «в роботі», коли агент почав, і «на ревʼю», коли є MR. Змінити можна в Налаштуваннях.</p>` : ""}
      ${t?.statusError ? `<p class="small warn">${h(t.statusError)}</p>` : ""}
      ${nav(t ? "Далі" : "Перевірити й далі")}`;
  }

  function viewCode() {
    const c = d.check;
    const found = d.found;
    const clone = cloneUrl();
    return `<h2>Де код ${h(d.project)} на цьому компʼютері?</h2>
      <p class="muted small">Тут агент створюватиме окрему копію (git worktree) для кожної задачі. Ваша робоча папка не зміниться.</p>
      ${found === null && clone ? `<p class="muted">Шукаю клон ${h(d.project)}…</p>` : ""}
      ${found?.length ? `<div class="wz-options">${found
        .map((p) => `<label class="wz-opt ${d.repoPath === p ? "on" : ""}"><input type="radio" name="found" value="${h(p)}" ${d.repoPath === p ? "checked" : ""}>
          <div><strong>${h(p)}</strong><div class="small muted">знайдено автоматично</div></div></label>`)
        .join("")}</div>` : ""}
      <label class="f">${found?.length ? "Або вкажіть іншу папку" : "Папка з репозиторієм"}
        <input type="text" data-k="repoPath" value="${h(d.repoPath)}" placeholder="${h(clone ? `${defaultFolder()} (Mako склонує сам)` : "~/projects/repo")}" ${found?.length ? "" : "autofocus"}>
        <span class="hint">${c ? (c.git ? `<span class="ok-text">✓ git-репозиторій${c.defaultBranch ? `, основна гілка <code>${h(c.defaultBranch)}</code>` : ""}</span>` : c.exists ? '<span class="warn">Папка є, але це не git-репозиторій</span>' : '<span class="warn">Такої папки немає</span>') : ""}</span></label>
      ${found?.length === 0 && clone && !d.repoPath.trim() ? `<p class="small muted">Клону на цьому компʼютері не знайшов. Нічого не вказуйте: перед першою задачею Mako склонує <code>${h(clone)}</code> у <code>${h(defaultFolder())}</code> (потрібен SSH-ключ для ${h(d.type === "github" ? "GitHub" : "GitLab")}).</p>` : ""}
      ${nav("Далі", { disabled: !(c?.git || (!d.repoPath.trim() && clone)) })}`;
  }

  function viewAgent() {
    const opt = (k, title, text) => `<label class="wz-opt ${d.executor === k ? "on" : ""}"><input type="radio" name="executor" value="${k}" ${d.executor === k ? "checked" : ""}>
      <div><strong>${title}</strong><div class="small muted">${text}</div></div></label>`;
    return `<h2>Хто виконує задачі?</h2>
      <div class="wz-options">
        ${opt("terminal", "Claude Code у терміналі Mako <span class=\"badge ok\">рекомендовано</span>", "Видно, що робить агент; можна відповісти на питання або втрутитись. Mako підсвітить, коли агент чекає на вас.")}
        ${opt("claude", "Claude Code у фоні", "Працює сам без терміналу; ви бачите результат і зміни.")}
      </div>
      ${doc && !doc.claude ? `<p class="notice warn small">Claude Code ще не встановлено: <code>npm install -g @anthropic-ai/claude-code</code>. Інших агентів (Codex, Orca, своя команда) можна додати в Налаштуваннях.</p>` : ""}
      ${nav(busy ? "Зберігаю…" : "Готово")}`;
  }

  function viewDone() {
    return `<div class="wz-done"><div class="wz-fin">🦈</div><h2>Mako готовий</h2>
      <p class="muted">Нові задачі з ${h(d.project)} зʼявлятимуться на дошці й у сповіщеннях. Натисніть «Взяти», і агент почне працювати.</p>
      <div class="adds wz-center"><a class="btn" href="/settings" style="margin:0">Налаштування</a><button type="button" data-act="more">Додати ще проєкт</button>
        <button type="button" class="primary" data-act="board">До задач</button></div></div>`;
  }

  // ---------- actions ----------

  async function next() {
    error = "";
    if (step === 0) step = 1;
    else if (step === 1) step = 2;
    else if (step === 2) {
      if (!d.test && !(await test())) return;
      step = 3;
      if (d.found === null && cloneUrl()) findClone();
    } else if (step === 3) step = 4;
    else if (step === 4) return save();
    render();
  }

  async function test() {
    busy = true;
    error = "";
    render();
    try {
      d.test = await api("POST", "/api/test-tracker", { tracker: tracker(), secrets: secrets() });
      return true;
    } catch (err) {
      error = err.message;
      return false;
    } finally {
      busy = false;
      render();
    }
  }

  async function findClone() {
    try {
      const r = await api("GET", `/api/find-repo?project=${encodeURIComponent(d.project)}`);
      d.found = r.paths;
      if (r.paths.length && !d.repoPath) {
        d.repoPath = r.paths[0];
        await checkPath();
      }
    } catch {
      d.found = [];
    }
    render();
  }

  let pathTimer;
  async function checkPath() {
    if (!d.repoPath.trim()) {
      d.check = null;
      return;
    }
    const want = d.repoPath;
    const c = await api("GET", `/api/check-path?path=${encodeURIComponent(want)}`).catch(() => null);
    if (want !== d.repoPath) return;
    d.check = c;
    if (c?.defaultBranch) d.baseBranch = c.defaultBranch;
  }

  async function save() {
    busy = true;
    render();
    try {
      const fresh = await api("GET", "/api/settings");
      const config = structuredClone(fresh.config);
      config.trackers ??= [];
      // No folder: Mako clones into ~/mako/projects; no base branch: the remote's default.
      const t = tracker(d.repoPath.trim() ? { repoPath: d.repoPath.trim() } : {});
      // Statuses the issue moves through, picked from the tracker's own list when the names are recognisable.
      const statuses = d.test?.statuses ?? [];
      const pick = (re) => statuses.find((x) => re.test(x));
      const onStart = pick(/progress|в роботі|doing|in work|розробці/i);
      const onSuccess = pick(/review|ревʼю|рев'ю|ревю|перевір|testing|qa/i);
      if (onStart || onSuccess) t.transitions = { onStart: onStart ?? null, onSuccess: onSuccess ?? null, onFailure: null };
      // Several trackers of one kind need their own ids.
      const ids = new Set(config.trackers.map((x) => x.id ?? x.type));
      if (ids.has(t.type)) {
        let n = 2;
        while (ids.has(`${t.type}-${n}`)) n++;
        t.id = `${t.type}-${n}`;
      }
      config.trackers.push(t);
      config.executors ??= {};
      if (d.executor === "terminal") config.executors.terminal ??= { type: "terminal" };
      if (d.executor === "claude") config.executors.claude ??= settings.config.executors?.claude ?? { type: "claude-headless" };
      if (!fresh.configured || !config.defaultExecutor) config.defaultExecutor = d.executor;
      await api("PUT", "/api/settings", { config, secrets: secrets() });
      settings = await api("GET", "/api/settings");
      step = STEPS.length;
    } catch (err) {
      error = err.message;
    } finally {
      busy = false;
      render();
    }
  }

  function restart() {
    Object.assign(d, { link: "", project: "", token: "", test: null, found: null, repoPath: "", check: null });
    step = 1;
    render();
  }

  root.addEventListener("click", async (e) => {
    const b = e.target.closest("button");
    if (!b) return;
    if (b.dataset.type) {
      d.type = b.dataset.type;
      d.test = null;
      return render();
    }
    const act = b.dataset.act;
    if (act === "next") return next();
    if (act === "back") {
      error = "";
      step--;
      return render();
    }
    if (act === "test") return test();
    if (act === "recheck") {
      doc = await api("GET", "/api/doctor").catch(() => doc);
      return render();
    }
    if (act === "more") return restart();
    if (act === "board") location.href = "/";
  });

  root.addEventListener("input", (e) => {
    const k = e.target.dataset.k;
    if (!k) return;
    d[k] = e.target.value;
    if (k === "link") {
      const p = parseLink(d.link);
      if (p.type) d.type = p.type;
      if (p.baseUrl !== undefined) d.baseUrl = p.baseUrl;
      if (p.project !== undefined) d.project = p.project;
      return rerender(e.target);
    }
    if (["project", "baseUrl", "token", "email", "label", "type"].includes(k)) d.test = null;
    if (k === "project") d.found = null;
    if (k === "repoPath") {
      clearTimeout(pathTimer);
      pathTimer = setTimeout(async () => {
        await checkPath();
        rerender(root.querySelector('[data-k="repoPath"]'));
      }, 250);
      return;
    }
    // Only the "Далі" state depends on these fields: update it without redrawing the inputs.
    rerender(e.target);
  });

  root.addEventListener("change", (e) => {
    const t = e.target;
    if (t.name === "mode") {
      d.mode = t.value;
      d.test = null;
    } else if (t.name === "executor") d.executor = t.value;
    else if (t.name === "found") {
      d.repoPath = t.value;
      checkPath().then(render);
    } else return;
    render();
  });

  root.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && e.target.matches("input[type=text], input[type=password]")) {
      e.preventDefault();
      const nextBtn = root.querySelector('[data-act="next"]');
      if (nextBtn && !nextBtn.disabled) next();
    }
  });

  /** Redraws while keeping focus and caret in the field being typed into. */
  function rerender(input) {
    const key = input?.dataset.k;
    const start = input?.selectionStart;
    render();
    if (!key) return;
    const again = root.querySelector(`[data-k="${key}"]`);
    if (again) {
      again.focus();
      try {
        again.setSelectionRange(start, start);
      } catch {
        // not a text field
      }
    }
  }

  (async () => {
    render();
    [settings, doc] = await Promise.all([api("GET", "/api/settings").catch(() => null), api("GET", "/api/doctor").catch(() => ({}))]);
    if (settings?.configured && step === 0) step = 1; // adding another project: the machine is already checked
    if (step === 3 && d.found === null && cloneUrl()) findClone();
    render();
  })();
})();
