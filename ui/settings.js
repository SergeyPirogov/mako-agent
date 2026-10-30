// Settings page: edits mako.config.json and .env through the local API. No build step, no dependencies.
(() => {
  "use strict";
  const TOKEN = document.querySelector('meta[name="mako-token"]').content;
  const root = document.getElementById("app");

  let model; // { general, executors: [{name, conf}], trackers: [{conf, projects: [{key, rest, repos: [{name, conf}]}]}] }
  let secrets = {}; // name -> { set, value? }
  let secretEdits = {}; // name -> string | null (null = remove)
  let meta = {}; // { configured, problem, configPath }
  let status = null; // { kind: "ok" | "err" | "warn", text }
  let tests = {}; // tracker index -> { loading } | { result } | { error }
  let dirty = false;
  let saving = false;
  const openState = new Set(); // <details> opened by the user
  const closedState = new Set(); // <details> closed by the user
  const openExecs = new Set(); // executor rows expanded
  const pathChecks = new Map(); // "path|base" -> result of /api/check-path

  const TRACKERS = { jira: "Jira", github: "GitHub", gitlab: "GitLab" };
  const EXECUTORS = {
    terminal: "Термінал у Mako (можна втрутитись)",
    "claude-headless": "Claude Code у фоні",
    orca: "Orca",
    command: "Своя команда",
  };
  const DEFAULT_LABELS = { trigger: "claude-agent" };
  const CLAUDE_ARGS = [
    "--permission-mode", "acceptEdits",
    "--allowedTools", "Bash(git:*) Bash(npm:*) Bash(npx:*) Bash(node:*) Bash(pnpm:*) Bash(yarn:*) Bash(bun:*) Read Edit Write Glob Grep",
    "--max-turns", "80",
  ];

  // ---------- API ----------

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

  async function load() {
    try {
      const data = await api("GET", "/api/settings");
      meta = { configured: data.configured, problem: data.problem, configPath: data.configPath };
      secrets = data.secrets;
      secretEdits = {};
      model = toModel(data.config);
      dirty = false;
      render();
    } catch (err) {
      root.innerHTML = `<div class="notice err">Не вдалося завантажити налаштування: ${h(err.message)}</div>`;
    }
  }

  // ---------- raw config <-> editable model ----------

  function toModel(raw) {
    const cfg = structuredClone(raw || {});
    if (!cfg.executors) cfg.executors = { claude: { type: "claude-headless", ...(cfg.claude || {}) } };
    delete cfg.claude;
    const executors = Object.entries(cfg.executors).map(([name, conf]) => ({ name, conf }));
    const trackers = (cfg.trackers || []).map((t) => {
      const { projects = {}, ...conf } = t;
      return {
        conf,
        projects: Object.entries(projects).map(([key, p]) => {
          if (!p.repos) p = { repos: { [key.split("/").pop()]: p } }; // short form: the project is one repo
          const { repos, ...rest } = p;
          return { key, rest, repos: Object.entries(repos).map(([name, conf]) => ({ name, conf })) };
        }),
      };
    });
    delete cfg.executors;
    delete cfg.trackers;
    return { general: cfg, executors, trackers };
  }

  function toRaw(m) {
    const raw = structuredClone(m.general);
    raw.executors = Object.fromEntries(m.executors.map((e) => [e.name.trim(), e.conf]));
    raw.trackers = m.trackers.map((t) => ({
      ...t.conf,
      projects: Object.fromEntries(
        t.projects.map((p) => [
          p.key.trim(),
          { ...p.rest, repos: Object.fromEntries(p.repos.map((r) => [repoName(r, p), r.conf])) },
        ]),
      ),
    }));
    return clean(raw);
  }

  function repoName(r, p) {
    const fromPath = (r.conf.repoPath || "").replace(/[\\/]+$/, "").split(/[\\/]/).pop();
    return r.name.trim() || fromPath || p.key.split("/").pop() || "repo";
  }

  /** Drops empty values so Mako's defaults apply. */
  function clean(v) {
    if (Array.isArray(v)) return v.map(clean);
    if (v && typeof v === "object") {
      const out = {};
      for (const [k, x] of Object.entries(v)) {
        const c = clean(x);
        if (c === undefined || c === "") continue;
        if (c && typeof c === "object" && !Array.isArray(c) && !Object.keys(c).length && k !== "projects" && k !== "repos") continue;
        out[k] = c;
      }
      return out;
    }
    return v;
  }

  function problems(m) {
    const errs = [];
    const dup = (list, what) => {
      const seen = new Set();
      for (const n of list) {
        if (!n) errs.push(`${what}: порожня назва`);
        else if (seen.has(n)) errs.push(`${what}: «${n}» повторюється`);
        seen.add(n);
      }
    };
    dup(m.executors.map((e) => e.name.trim()), "Виконавці");
    if (!m.trackers.length) errs.push("Додайте хоча б один трекер");
    m.trackers.forEach((t, ti) => {
      const name = `${TRACKERS[t.conf.type]} #${ti + 1}`;
      if (!t.projects.length) errs.push(`${name}: додайте проєкт`);
      dup(t.projects.map((p) => p.key.trim()), `${name}, проєкти`);
      t.projects.forEach((p) => {
        p.repos.forEach((r) => {
          if (!r.conf.repoPath) errs.push(`${name}, ${p.key || "проєкт"}: вкажіть шлях до репозиторію`);
        });
        dup(p.repos.map((r) => repoName(r, p)), `${name}, ${p.key || "проєкт"}, репозиторії`);
      });
    });
    return errs;
  }

  // ---------- helpers ----------

  function h(s) {
    return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
  }

  function getAt(obj, path) {
    return path.split(".").reduce((o, k) => (o == null ? undefined : o[k]), obj);
  }

  function setAt(obj, path, value) {
    const keys = path.split(".");
    let o = obj;
    for (const k of keys.slice(0, -1)) {
      if (o[k] == null || typeof o[k] !== "object") o[k] = {};
      o = o[k];
    }
    const last = keys[keys.length - 1];
    if (value === undefined) delete o[last];
    else o[last] = value;
  }

  function display(value, type) {
    if (type === "lines") return (value || []).join("\n");
    if (type === "csv") return (value || []).join(", ");
    return value ?? "";
  }

  function readValue(el) {
    const type = el.dataset.type || "string";
    if (type === "bool") return el.checked ? true : undefined;
    if (type === "nullflag") return el.checked ? null : undefined;
    if (type === "booltrue") return el.checked ? undefined : false;
    const v = el.value;
    if (type === "number") return v === "" ? undefined : Number(v);
    if (type === "lines" || type === "csv") {
      const items = v.split(type === "lines" ? "\n" : ",").map((x) => x.trim()).filter(Boolean);
      return items.length ? items : undefined;
    }
    return v.trim() === "" ? undefined : v;
  }

  /** One labelled form control bound to a path in the model. */
  function field(o) {
    const { label, path, kind = "text", type = "string", placeholder = "", hint = "", options, wide, refresh, rerender, list } = o;
    const value = getAt(model, path);
    const attrs = `data-path="${h(path)}" data-type="${type}"${refresh ? " data-refresh" : ""}${rerender ? " data-rerender" : ""}`;
    let control;
    if (kind === "check") {
      const checked = type === "booltrue" ? value !== false : type === "nullflag" ? value === null : Boolean(value);
      return `<label class="check${wide ? " wide" : ""}"><input type="checkbox" ${attrs}${checked ? " checked" : ""}> ${h(label)}</label>`;
    } else if (kind === "select") {
      control = `<select ${attrs}>${options
        .map(([v, l]) => `<option value="${h(v)}"${String(value ?? "") === v ? " selected" : ""}>${h(l)}</option>`)
        .join("")}</select>`;
    } else if (kind === "textarea") {
      const text = display(value, type);
      const rows = Math.min(12, Math.max(3, String(text || placeholder).split("\n").length + 1));
      control = `<textarea ${attrs} rows="${rows}" placeholder="${h(placeholder)}" spellcheck="false">${h(text)}</textarea>`;
    } else {
      control = `<input type="${kind}" ${attrs} value="${h(display(value, type))}" placeholder="${h(placeholder)}" spellcheck="false"${list ? ` list="${h(list)}"` : ""}>`;
    }
    return `<label class="f${wide ? " wide" : ""}">${h(label)}${control}${hint ? `<span class="hint">${hint}</span>` : ""}</label>`;
  }

  function button(label, action, data = {}, cls = "") {
    const attrs = Object.entries(data).map(([k, v]) => ` data-${k}="${h(v)}"`).join("");
    return `<button type="button" class="${cls}" data-action="${action}"${attrs}>${label}</button>`;
  }

  function uniqueName(base, taken) {
    if (!taken.includes(base)) return base;
    let i = 2;
    while (taken.includes(`${base}-${i}`)) i++;
    return `${base}-${i}`;
  }

  // ---------- rendering ----------

  /** Re-renders the page, keeping focus and the caret in the field the user is in. */
  function render() {
    const active = document.activeElement;
    const key = active && root.contains(active) ? fieldKey(active) : null;
    const sel = key && typeof active.selectionStart === "number" ? [active.selectionStart, active.selectionEnd] : null;
    renderNow();
    if (!key) return;
    const el = root.querySelector(key);
    if (!el) return;
    el.focus({ preventScroll: true });
    if (sel) {
      try {
        el.setSelectionRange(sel[0], sel[1]);
      } catch {
        // inputs like number or checkbox have no caret
      }
    }
  }

  function fieldKey(el) {
    if (el.dataset.secret) return `[data-secret="${CSS.escape(el.dataset.secret)}"]`;
    if (el.dataset.path && el.type !== "radio") return `[data-path="${CSS.escape(el.dataset.path)}"]`;
    return null;
  }

  function renderNow() {
    const banner = !meta.configured
      ? `<div class="notice">👋 Mako ще не налаштований. Додайте трекер, вкажіть репозиторій і натисніть «Зберегти», після цього Mako одразу почне працювати.</div>`
      : meta.problem
        ? `<div class="notice err">Mako зараз не працює: ${h(meta.problem)}</div>`
        : "";
    const sections = [
      ["s-trackers", "Трекери і репозиторії", trackersSection()],
      ["s-executors", "Хто виконує", executorsSection()],
      ["s-work", "Задачі й ліміти", workSection()],
      ["s-notify", "Сповіщення", notifySection()],
      ["s-advanced", "Додатково", advancedSection()],
    ];
    root.innerHTML = `<div class="settings">
      <nav class="snav">${sections.map(([id, title]) => `<a href="#${id}" data-nav="${id}">${h(title)}</a>`).join("")}</nav>
      <div class="scontent"><h1>Налаштування</h1>${banner}
        ${sections.map(([id, , html]) => `<section id="${id}" class="sblock">${html}</section>`).join("")}
      </div></div>${saveBar()}`;
    spy();
    checkPaths();
  }

  /** Highlights the section in view in the side navigation. */
  let observer;
  function spy() {
    observer?.disconnect();
    const links = new Map([...root.querySelectorAll("[data-nav]")].map((a) => [a.dataset.nav, a]));
    observer = new IntersectionObserver(
      (entries) => {
        const visible = entries.filter((e) => e.isIntersecting).sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top)[0];
        if (!visible) return;
        for (const [id, a] of links) a.classList.toggle("on", id === visible.target.id);
      },
      { rootMargin: "-60px 0px -60% 0px" },
    );
    root.querySelectorAll("section.sblock").forEach((el) => observer.observe(el));
  }

  /** Collapsible group whose open state survives re-renders. */
  function details(key, summary, body, defaultOpen = false) {
    const open = openState.has(key) || (defaultOpen && !closedState.has(key));
    return `<details class="adv" data-key="${h(key)}"${open ? " open" : ""}><summary>${summary}</summary><div class="grid">${body}</div></details>`;
  }

  function secretField(name, label, placeholder = "", hint = "") {
    const s = secrets[name] || {};
    const hidden = name.endsWith("_TOKEN");
    const edited = name in secretEdits;
    const value = edited ? secretEdits[name] ?? "" : hidden ? "" : s.value ?? "";
    const ph = hidden && s.set && !edited ? "•••••••• збережено" : placeholder;
    const state =
      edited && secretEdits[name] === null ? `<span class="hint err">буде видалено після збереження</span>`
      : hidden && !s.set && !edited ? `<span class="hint warn">ще не задано</span>` : "";
    return `<label class="f">${h(label)}
      <span class="secret"><input type="${hidden ? "password" : "text"}" data-secret="${name}" value="${h(value)}" placeholder="${h(ph)}" autocomplete="off" spellcheck="false">
      ${s.set || edited ? button("×", "clear-secret", { name }, "link") : ""}</span>
      ${state}${hint ? `<span class="hint">${hint}</span>` : ""}</label>`;
  }

  function trackersSection() {
    const cards = model.trackers.map(trackerCard).join("");
    return `<div class="shead"><h2>Трекери і репозиторії</h2>
        <p class="muted">Звідки Mako бере задачі і в які локальні репозиторії їх віддає.</p></div>
      ${cards || `<p class="muted">Трекерів ще немає.</p>`}
      <div class="adds"><span class="muted">Додати трекер:</span>
        ${button("+ Jira", "add-tracker", { type: "jira" })}${button("+ GitHub", "add-tracker", { type: "github" })}${button("+ GitLab", "add-tracker", { type: "gitlab" })}</div>`;
  }

  function trackerCard(t, ti) {
    const p = `trackers.${ti}.conf`;
    const type = t.conf.type;
    let main = "";
    let advanced = "";
    let host = "";
    if (type === "jira") {
      host = secrets.JIRA_BASE_URL?.value || t.conf.baseUrl || "";
      main = `
        ${secretField("JIRA_BASE_URL", "Адреса Jira", "https://company.atlassian.net")}
        ${secretField("JIRA_EMAIL", "Email в Atlassian", "you@company.com")}
        ${secretField("JIRA_API_TOKEN", "API-токен", "", 'Створити: <a href="https://id.atlassian.com/manage-profile/security/api-tokens" target="_blank" rel="noreferrer">id.atlassian.com → API tokens</a>')}
        ${field({ label: "Які задачі віддавати агенту (JQL)", path: `${p}.jql`, kind: "textarea", wide: true,
          placeholder: 'assignee = currentUser() AND labels = claude-agent AND statusCategory = "To Do"',
          hint: "Порядок задає <code>ORDER BY</code>: пріоритетні задачі пропонуються першими" })}
        ${field({ label: "Статус, коли агент почав", path: `${p}.transitions.onStart`, placeholder: "не змінювати" })}
        ${field({ label: "Статус, коли є PR", path: `${p}.transitions.onSuccess`, placeholder: "не змінювати" })}`;
      advanced = `
        ${field({ label: "Статус при помилці", path: `${p}.transitions.onFailure`, placeholder: "не змінювати" })}
        ${field({ label: "Інша адреса Jira для цього трекера", path: `${p}.baseUrl`, placeholder: "як у секреті вище" })}`;
    } else {
      const isLab = type === "gitlab";
      const base = (isLab ? t.conf.baseUrl : "") || (isLab ? "https://gitlab.com" : "https://github.com");
      host = base.replace(/^https?:\/\//, "");
      const tokenHint = isLab
        ? `Personal access token зі scope <code>api</code>: <a href="${h(base.replace(/\/$/, ""))}/-/user_settings/personal_access_tokens" target="_blank" rel="noreferrer">створити</a>`
        : "Необовʼязково, якщо виконано <code>gh auth login</code>. Fine-grained: Issues → Read and write";
      const statuses = tests[ti]?.result?.statuses;
      const statusList = statuses ? `<datalist id="statuses-${ti}">${statuses.map((x) => `<option value="${h(x)}">`).join("")}</datalist>` : "";
      const statusHint = statuses ? `Статуси проєкту: ${statuses.map((x) => `<code>${h(x)}</code>`).join(", ")}` : "Назва статусу задачі в GitLab, напр. <code>In progress</code>. Порожньо: не змінювати";
      main = `
        ${isLab ? field({ label: "Адреса GitLab", path: `${p}.baseUrl`, placeholder: "https://gitlab.com", refresh: true }) : ""}
        ${secretField(isLab ? "GITLAB_TOKEN" : "GITHUB_TOKEN", "Токен", "", tokenHint)}
        ${field({ label: "Лише призначені на", path: `${p}.assignee`, placeholder: "будь-хто",
          hint: `Ваш нік у ${isLab ? "GitLab" : "GitHub"} без <code>@</code>, або <code>me</code>: Mako візьме його з токена` })}
        ${t.conf.labels?.trigger === null
          ? `<div class="f"><span>Мітка для агента</span><span class="muted small" style="padding:8px 0">не потрібна: беруться всі відкриті задачі, призначені на вас</span></div>`
          : field({ label: "Мітка для агента", path: `${p}.labels.trigger`, placeholder: DEFAULT_LABELS.trigger, hint: "Задачі з цією міткою потрапляють до Mako. Інших міток Mako не ставить" })}
        ${field({ label: "Брати всі мої задачі, без мітки", path: `${p}.labels.trigger`, kind: "check", type: "nullflag", wide: true, rerender: true })}
        ${isLab ? `${statusList}
          ${field({ label: "Статус, коли агент почав", path: `${p}.transitions.onStart`, placeholder: "не змінювати", list: `statuses-${ti}`, hint: statusHint })}
          ${field({ label: "Статус, коли є MR", path: `${p}.transitions.onSuccess`, placeholder: "не змінювати", list: `statuses-${ti}`,
            hint: "Після мерджу MR із <code>Closes #N</code> задача закриється сама" })}` : ""}`;
      advanced = `
        ${isLab ? field({ label: "Статус при помилці", path: `${p}.transitions.onFailure`, placeholder: "не змінювати", list: `statuses-${ti}` }) : ""}
        ${field({ label: "Мітка «в роботі» (необовʼязково)", path: `${p}.labels.inProgress`, placeholder: "не ставити" })}
        ${field({ label: "Мітка «готово» (необовʼязково)", path: `${p}.labels.done`, placeholder: "не ставити" })}
        ${field({ label: "Мітка «помилка» (необовʼязково)", path: `${p}.labels.failed`, placeholder: "не ставити" })}
        ${isLab ? "" : field({ label: "API (для GitHub Enterprise)", path: `${p}.apiUrl`, placeholder: "https://api.github.com" })}`;
    }
    const test = tests[ti];
    const badge = !test
      ? `<span class="badge">не перевірено</span>`
      : test.loading ? `<span class="badge">перевіряю…</span>`
      : test.error ? `<span class="badge err">помилка</span>`
      : `<span class="badge ok">підключено</span>`;
    const testHtml = !test || test.loading
      ? ""
      : test.error
        ? `<div class="result err">✗ ${h(test.error)}</div>`
        : `<div class="result"><span class="ok-text">✓ Підключено.</span> Задач для агента: <b>${test.result.count}</b>${
            test.result.sample.length ? `<ul>${test.result.sample.map((x) => `<li><code>${h(x.id)}</code> ${h(x.title)}</li>`).join("")}</ul>` : ""
          }${!test.result.count && test.result.filter ? `<div class="muted">Шукав: ${h(test.result.filter)}. Якщо ваших задач тут немає, перевірте нік і мітку.</div>` : ""}${test.result.statusError ? `<div class="warn">⚠ ${h(test.result.statusError)}</div>` : ""}</div>`;
    return `<div class="card tracker">
      <div class="head"><div class="title"><h3 class="tname">${TRACKERS[type]}</h3>${host ? `<span class="muted">${h(host)}</span>` : ""}${badge}</div>
        <div class="tools">${button("Перевірити підключення", "test-tracker", { ti })}${button("Видалити", "remove-tracker", { ti }, "link danger")}</div></div>
      <div class="grid">${main}</div>${testHtml}
      ${details(`tracker-${ti}-adv`, "Додатково", advanced)}
      <div class="subhead">Проєкти → локальні репозиторії</div>
      ${t.projects.map((proj, pi) => projectCard(t, ti, proj, pi)).join("")}
      <div class="adds">${button("+ проєкт", "add-project", { ti })}</div>
    </div>`;
  }

  function projectCard(t, ti, proj, pi) {
    const type = t.conf.type;
    const p = `trackers.${ti}.projects.${pi}`;
    const keyLabel = type === "jira" ? "Ключ проєкту Jira" : type === "github" ? "Репозиторій GitHub" : "Проєкт GitLab";
    const keyPlaceholder = type === "jira" ? "PROJ" : type === "github" ? "my-org/my-repo" : "group/project";
    const multi = proj.repos.length > 1;
    return `<div class="card project"><div class="head"><div class="grid" style="flex:1">
        ${field({ label: keyLabel, path: `${p}.key`, placeholder: keyPlaceholder })}
        ${multi
          ? field({ label: "Репозиторій за замовчуванням", path: `${p}.rest.default`, kind: "select",
              options: [["", "немає (лише мітки й правила)"], ...proj.repos.map((r) => [repoName(r, proj), repoName(r, proj)])],
              hint: "Куди йдуть задачі без мітки <code>repo:</code> і без збігу правил" })
          : ""}
      </div>${t.projects.length > 1 ? button("Видалити", "remove-project", { ti, pi }, "link danger") : ""}</div>
      ${proj.repos.map((r, ri) => repoCard(t, ti, proj, pi, r, ri)).join("")}
      <div class="adds">${button("+ ще один репозиторій", "add-repo", { ti, pi }, "link")}
        ${multi ? "" : `<span class="muted small">якщо задачі проєкту йдуть у різні репозиторії</span>`}</div>
    </div>`;
  }

  function repoCard(t, ti, proj, pi, r, ri) {
    const p = `trackers.${ti}.projects.${pi}.repos.${ri}`;
    const multi = proj.repos.length > 1;
    const defaultExec = model.general.defaultExecutor || model.executors[0]?.name || "";
    const check = pathCheckKey(r.conf.repoPath, r.conf.baseBranch);
    return `<div class="card repo"><div class="head"><strong>📁 ${h(repoName(r, proj))}</strong>
        ${multi ? button("Видалити", "remove-repo", { ti, pi, ri }, "link danger") : ""}</div>
      <div class="grid">
        ${field({ label: "Шлях до локального репозиторію", path: `${p}.conf.repoPath`, wide: true, placeholder: "~/work/my-repo", refresh: true,
          hint: `<span class="pathcheck" data-check="${h(check)}">${pathCheckHtml(check)}</span>` })}
        ${field({ label: "Базова гілка", path: `${p}.conf.baseBranch`, placeholder: "main", refresh: true })}
        ${field({ label: "Хто виконує", path: `${p}.conf.executor`, kind: "select",
          options: [["", `основний (${defaultExec})`], ...model.executors.map((e) => [e.name, `${e.name} · ${EXECUTORS[e.conf.type] ?? e.conf.type}`])] })}
        ${multi ? field({ label: "Сюди йдуть задачі з мітками", path: `${p}.conf.match.labels`, type: "csv", placeholder: "ui, frontend" }) : ""}
        ${multi && t.conf.type === "jira" ? field({ label: "…або з компонентами Jira", path: `${p}.conf.match.components`, type: "csv", placeholder: "Frontend" }) : ""}
      </div>
      ${details(`repo-${ti}-${pi}-${ri}`, "Додатково", `
        ${field({ label: "Підготовка worktree", path: `${p}.conf.setup`, wide: true, placeholder: "auto: за lock-файлом (pnpm install, npm ci, …)",
          hint: "Команда в новому worktree до старту агента, щоб працювали тести, лінтери й git-хуки. <code>none</code>: нічого не запускати" })}
        ${field({ label: "Додаткові інструкції агенту", path: `${p}.conf.extraInstructions`, wide: true, placeholder: "Run `pnpm lint` before committing." })}
        ${field({ label: "Pull request", path: `${p}.conf.pullRequest`, kind: "select",
          options: [["", "автоматично (за origin)"], ["github", "GitHub (gh)"], ["gitlab", "GitLab (MR)"], ["none", "лише push"]] })}
        ${field({ label: "Назва", path: `${p}.name`, placeholder: repoName({ ...r, name: "" }, proj), refresh: true,
          hint: multi ? `Мітка <code>repo:${h(repoName(r, proj))}</code> направляє задачу сюди` : "Для мітки <code>repo:назва</code>" })}
        ${field({ label: "Папка для worktree", path: `${p}.conf.worktreesDir`, wide: true, placeholder: "поруч із репозиторієм: ../.worktrees/<назва>" })}`)}
    </div>`;
  }

  // ---- live check of repository paths ----

  function pathCheckKey(repoPath, base) {
    return repoPath ? `${repoPath}|${base || "main"}` : "";
  }

  function pathCheckHtml(key) {
    if (!key) return "";
    const c = pathChecks.get(key);
    if (!c || c === "pending") return `<span class="muted">перевіряю шлях…</span>`;
    if (c.error) return "";
    const base = key.split("|")[1];
    if (!c.exists) return `<span class="err">✗ Папки не існує: ${h(c.resolved)}</span>`;
    if (!c.git) return `<span class="warn">⚠ Це не git-репозиторій</span>`;
    if (!c.hasBase) return `<span class="warn">⚠ Гілки <code>origin/${h(base)}</code> не знайдено: перевірте назву або зробіть <code>git fetch</code></span>`;
    return `<span class="ok-text">✓ git-репозиторій, гілка <code>origin/${h(base)}</code> є</span>`;
  }

  function checkPaths() {
    for (const el of root.querySelectorAll("[data-check]")) {
      const key = el.dataset.check;
      if (!key || pathChecks.has(key)) continue;
      pathChecks.set(key, "pending");
      const [repoPath, base] = key.split("|");
      api("GET", `/api/check-path?path=${encodeURIComponent(repoPath)}&base=${encodeURIComponent(base)}`)
        .then((r) => pathChecks.set(key, r))
        .catch(() => pathChecks.set(key, { error: true }))
        .finally(() => root.querySelectorAll(`[data-check="${CSS.escape(key)}"]`).forEach((n) => (n.innerHTML = pathCheckHtml(key))));
    }
  }

  function executorsSection() {
    const defaultExec = model.general.defaultExecutor || model.executors[0]?.name;
    const usage = (name) =>
      model.trackers.flatMap((t) => t.projects.flatMap((p) => p.repos)).filter((r) => (r.conf.executor || defaultExec) === name).length;
    const rows = model.executors
      .map((e, ei) => {
        const p = `executors.${ei}.conf`;
        const type = e.conf.type;
        const open = openExecs.has(ei);
        let fields = "";
        if (type === "terminal") {
          fields = `
            ${field({ label: "Команда агента", path: `${p}.command`, placeholder: "claude", hint: "Повний шлях, якщо Mako не знаходить команду. На Windows: <code>claude.cmd</code>" })}
            ${field({ label: "Аргументи (по одному на рядок)", path: `${p}.args`, type: "lines", kind: "textarea", wide: true,
              placeholder: "{prompt}", hint: "За замовчуванням лише <code>{prompt}</code>" })}
            ${field({ label: "Завершувати сам, якщо я не втручався", path: `${p}.autoFinish`, kind: "check", type: "bool", wide: true })}`;
        } else if (type === "claude-headless") {
          fields = `
            ${field({ label: "Команда", path: `${p}.command`, placeholder: "claude" })}
            ${field({ label: "Аргументи (по одному на рядок)", path: `${p}.args`, type: "lines", kind: "textarea", wide: true,
              hint: "Без людини Claude не може спитати дозволу: дозволено лише те, що в <code>--allowedTools</code>" })}`;
        } else if (type === "orca") {
          fields = `
            ${field({ label: "Агент в Orca", path: `${p}.agent`, placeholder: "claude", hint: "claude, codex, cursor, opencode…" })}
            ${field({ label: "Setup-хуки репозиторію", path: `${p}.setup`, kind: "select", options: [["", "як у репозиторії"], ["run", "запускати"], ["skip", "пропускати"]] })}
            ${field({ label: "Команда", path: `${p}.command`, placeholder: "orca", hint: "Mako шукає її й усередині Orca.app" })}
            ${field({ label: "Привʼязувати задачу до workspace", path: `${p}.linkIssue`, kind: "check", type: "booltrue" })}`;
        } else {
          fields = `
            ${field({ label: "Режим", path: `${p}.mode`, kind: "select", rerender: true,
              options: [["", "Mako чекає завершення"], ["handoff", "Запустити й передати мені"]] })}
            ${field({ label: "Назва для сповіщень", path: `${p}.label`, placeholder: "Codex" })}
            ${field({ label: "Команда", path: `${p}.command`, placeholder: "codex" })}
            ${field({ label: "Аргументи (по одному на рядок)", path: `${p}.args`, type: "lines", kind: "textarea", wide: true,
              placeholder: "exec\n--full-auto\n{prompt}",
              hint: "Підстановки: {prompt} {promptFile} {worktree} {branch} {taskId} {title} {url} {repoPath}" })}
            ${field({ label: "Створювати git worktree", path: `${p}.worktree`, kind: "check", type: "booltrue" })}`;
        }
        const isDefault = e.name === defaultExec;
        const used = usage(e.name);
        return `<div class="exec${open ? " open" : ""}">
          <div class="exec-row">
            <button class="exec-toggle" data-action="toggle-exec" data-ei="${ei}" aria-expanded="${open}">
              <span class="caret">${open ? "▾" : "▸"}</span><strong>${h(e.name || "без назви")}</strong>
              <span class="muted">${h(EXECUTORS[type] ?? type)}</span></button>
            <span class="muted small">${used ? `репозиторіїв: ${used}` : "не використовується"}</span>
            ${isDefault ? `<span class="badge ok">основний</span>` : button("Зробити основним", "make-default", { ei }, "link")}
            ${model.executors.length > 1 ? button("Видалити", "remove-executor", { ei }, "link danger") : ""}
          </div>
          ${open ? `<div class="grid exec-body">
            ${field({ label: "Назва", path: `executors.${ei}.name`, refresh: true })}
            ${field({ label: "Тип", path: `${p}.type`, kind: "select", rerender: true, options: Object.entries(EXECUTORS) })}
            ${fields}</div>` : ""}
        </div>`;
      })
      .join("");
    return `<div class="shead"><h2>Хто виконує задачі</h2>
        <p class="muted">Як запускається агент. «Основний» діє для всіх репозиторіїв, де не вибрано інший.</p></div>
      <div class="card execs">${rows}</div>
      <div class="adds"><span class="muted">Додати:</span>
        ${button("+ Термінал", "add-executor", { type: "terminal" })}${button("+ Claude Code у фоні", "add-executor", { type: "claude-headless" })}${button("+ Orca", "add-executor", { type: "orca" })}${button("+ Своя команда", "add-executor", { type: "command" })}</div>`;
  }

  function workSection() {
    return `<div class="shead"><h2>Задачі й ліміти</h2></div>
      <div class="card"><div class="grid">
        ${field({ label: "Коли зʼявляється нова задача", path: "general.notifications.autoTake", kind: "select", type: "string",
          options: [["", "питати: «Взяти» / «Відкласти»"], ["true", "брати без питань"]] })}
        ${field({ label: "Ліміт задач у роботі (WIP)", path: "general.wipLimit", kind: "number", type: "number", placeholder: "без ліміту",
          hint: "Поки стільки задач у роботі, Mako не шукає нових. Порожньо: без ліміту" })}
        ${field({ label: "Шукати задачі кожні, с", path: "general.pollIntervalSec", kind: "number", type: "number", placeholder: "60" })}
        ${field({ label: "«Відкласти» на, хв", path: "general.notifications.snoozeMin", kind: "number", type: "number", placeholder: "60" })}
        ${field({ label: "Агентів одночасно", path: "general.maxConcurrent", kind: "number", type: "number",
          placeholder: String(model.general.wipLimit || 1), hint: "За замовчуванням дорівнює WIP-ліміту" })}
        ${field({ label: "Ліміт часу на задачу, хв", path: "general.taskTimeoutMin", kind: "number", type: "number", placeholder: "45", hint: "Для фонових виконавців" })}
      </div></div>`;
  }

  function notifySection() {
    return `<div class="shead"><h2>Сповіщення</h2></div>
      <div class="card"><div class="grid">
        ${field({ label: "Показувати сповіщення на десктопі", path: "general.notifications.enabled", kind: "check", type: "booltrue", wide: true })}
        ${field({ label: "Нагадувати, що агент чекає, кожні, хв", path: "general.notifications.remindMin", kind: "number", type: "number", placeholder: "5", hint: "0: лише одне сповіщення" })}
        ${field({ label: "Сповіщення без відповіді зникає через, с", path: "general.notifications.timeoutSec", kind: "number", type: "number", placeholder: "600" })}
      </div></div>`;
  }

  function advancedSection() {
    return `<div class="shead"><h2>Додатково</h2></div>
      <div class="card"><div class="grid">
        ${field({ label: "Шукати PR переданих задач кожні, хв", path: "general.handoffCheckMin", kind: "number", type: "number", placeholder: "5", hint: "Для Orca й інших зовнішніх інструментів. 0 вимикає" })}
        ${field({ label: "Порт вебінтерфейсу", path: "general.notifications.inboxPort", kind: "number", type: "number", placeholder: "7777", hint: "Діє після перезапуску Mako" })}
      </div>
      <p class="muted small">Файл конфігу: <code>${h(meta.configPath || "")}</code>. Токени лежать поруч у <code>.env</code>.</p></div>`;
  }

  function saveBar() {
    const msg = status
      ? `<span class="msg ${status.kind}">${status.text}</span>`
      : `<span class="msg muted">${dirty ? "● Є незбережені зміни" : "Усі зміни збережено"}</span>`;
    return `<div class="savebar"><div class="in">${msg}
      ${dirty ? button("Скасувати зміни", "reset") : ""}
      ${button(saving ? "Зберігаю…" : "Зберегти", "save", {}, "primary")}<span class="muted small kbd">${navigator.platform.includes("Mac") ? "⌘S" : "Ctrl+S"}</span></div></div>`;
  }

  // ---------- actions ----------

  const actions = {
    "add-tracker"({ type }) {
      const conf = type === "jira"
        ? { type, jql: 'assignee = currentUser() AND labels = claude-agent AND statusCategory = "To Do"', transitions: { onStart: "In Progress", onSuccess: "In Review" } }
        : { type };
      model.trackers.push({ conf, projects: [{ key: "", rest: {}, repos: [{ name: "", conf: {} }] }] });
      setTimeout(() => root.querySelectorAll(".card.tracker")[model.trackers.length - 1]?.scrollIntoView({ behavior: "smooth", block: "start" }), 0);
    },
    async "remove-tracker"({ ti }) {
      const ok = await makoConfirm({
        title: `Видалити трекер ${TRACKERS[model.trackers[ti].conf.type]}?`,
        body: "Його проєкти й репозиторії зникнуть з налаштувань після збереження. Токен лишиться в .env.",
        confirm: "Видалити",
        tone: "danger",
      });
      if (!ok) return "skip";
      model.trackers.splice(ti, 1);
      tests = {};
    },
    "add-project"({ ti }) {
      model.trackers[ti].projects.push({ key: "", rest: {}, repos: [{ name: "", conf: {} }] });
    },
    "remove-project"({ ti, pi }) {
      model.trackers[ti].projects.splice(pi, 1);
    },
    "add-repo"({ ti, pi }) {
      model.trackers[ti].projects[pi].repos.push({ name: "", conf: {} });
    },
    "remove-repo"({ ti, pi, ri }) {
      const proj = model.trackers[ti].projects[pi];
      const removed = repoName(proj.repos[ri], proj);
      proj.repos.splice(ri, 1);
      if (proj.rest.default === removed) delete proj.rest.default;
    },
    "add-executor"({ type }) {
      const names = model.executors.map((e) => e.name);
      const base = { terminal: "terminal", "claude-headless": "claude", orca: "orca", command: "command" }[type];
      const conf =
        type === "claude-headless" ? { type, args: [...CLAUDE_ARGS] }
        : type === "orca" ? { type, agent: "claude" }
        : type === "terminal" ? { type }
        : { type, command: "", args: [] };
      model.executors.push({ name: uniqueName(base, names), conf });
      openExecs.add(model.executors.length - 1);
    },
    "remove-executor"({ ei }) {
      const [removed] = model.executors.splice(ei, 1);
      openExecs.clear();
      if (model.general.defaultExecutor === removed.name) delete model.general.defaultExecutor;
      for (const t of model.trackers) for (const p of t.projects) for (const r of p.repos) if (r.conf.executor === removed.name) delete r.conf.executor;
    },
    "toggle-exec"({ ei }) {
      openExecs.has(ei) ? openExecs.delete(ei) : openExecs.add(ei);
      render();
      return "skip";
    },
    "make-default"({ ei }) {
      model.general.defaultExecutor = model.executors[ei].name;
    },
    "clear-secret"({ name }) {
      secretEdits[name] = null;
    },
    reset() {
      status = null;
      load();
      return "skip";
    },
    async "test-tracker"({ ti }) {
      tests[ti] = { loading: true };
      render();
      try {
        const raw = toRaw({ general: {}, executors: [], trackers: [model.trackers[ti]] });
        tests[ti] = { result: await api("POST", "/api/test-tracker", { tracker: raw.trackers[0], secrets: secretEdits }) };
      } catch (err) {
        tests[ti] = { error: err.message };
      }
      render();
      return "skip";
    },
    async save() {
      const errs = problems(model);
      if (errs.length) {
        status = { kind: "err", text: h(errs[0]) + (errs.length > 1 ? ` (і ще ${errs.length - 1})` : "") };
        render();
        return "skip";
      }
      saving = true;
      status = null;
      render();
      try {
        const res = await api("PUT", "/api/settings", { config: toRaw(model), secrets: secretEdits });
        saving = false;
        await load();
        status = res.problem
          ? { kind: "err", text: `Збережено, але Mako не запустився: ${h(res.problem)}` }
          : res.warnings.length
            ? { kind: "warn", text: `✓ Збережено. Увага: ${h(res.warnings.join("; "))}` }
            : { kind: "ok", text: "✓ Збережено. Mako вже працює з новими налаштуваннями." };
      } catch (err) {
        saving = false;
        status = { kind: "err", text: `Не збережено: ${h(err.message)}` };
      }
      render();
      return "skip";
    },
  };

  // ---------- events ----------

  root.addEventListener("input", (e) => {
    const el = e.target;
    if (el.dataset.secret) {
      const name = el.dataset.secret;
      if (el.value === "" && name.endsWith("_TOKEN")) delete secretEdits[name];
      else secretEdits[name] = el.value;
    } else if (el.dataset.path) {
      let value = readValue(el);
      if (el.dataset.path === "general.notifications.autoTake") value = value === "true" ? true : undefined;
      setAt(model, el.dataset.path, value);
    } else return;
    dirty = true;
    status = null;
    if (el.dataset.rerender !== undefined) render();
    else document.querySelector(".savebar")?.replaceWith(htmlToNode(saveBar()));
  });

  // Names and paths update labels elsewhere on the page, so leaving such a field re-renders.
  // That waits until the mouse button is released: re-rendering between mousedown and mouseup
  // would replace the field or button being clicked, losing the click and the focus (and a pasted token).
  let pointerDown = false;
  let refreshPending = false;
  const flushRefresh = () => {
    if (!refreshPending || pointerDown) return;
    refreshPending = false;
    render();
  };
  document.addEventListener("pointerdown", () => (pointerDown = true), true);
  document.addEventListener("pointerup", () => {
    pointerDown = false;
    setTimeout(flushRefresh, 0); // after the click handler has run
  }, true);

  root.addEventListener("change", (e) => {
    if (e.target.dataset.refresh === undefined) return;
    refreshPending = true;
    setTimeout(flushRefresh, 0);
  });

  root.addEventListener("click", async (e) => {
    const btn = e.target.closest("button[data-action]");
    if (!btn) return;
    const data = { ...btn.dataset };
    for (const k of ["ti", "pi", "ri", "ei"]) if (k in data) data[k] = Number(data[k]);
    const result = await actions[data.action](data);
    if (result === "skip") return;
    dirty = true;
    status = null;
    render();
  });

  // <details> do not bubble "toggle"; remember what the user opened or closed so re-renders keep it.
  root.addEventListener(
    "toggle",
    (e) => {
      const key = e.target.dataset?.key;
      if (!key) return;
      if (e.target.open) openState.add(key), closedState.delete(key);
      else openState.delete(key), closedState.add(key);
    },
    true,
  );

  document.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") {
      e.preventDefault();
      if (!saving) actions.save();
    }
  });

  window.addEventListener("beforeunload", (e) => {
    if (dirty) e.preventDefault();
  });

  function htmlToNode(html) {
    const t = document.createElement("template");
    t.innerHTML = html.trim();
    return t.content.firstChild;
  }

  load();
})();
