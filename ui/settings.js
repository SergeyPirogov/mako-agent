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

  const TRACKERS = { jira: "Jira", github: "GitHub", gitlab: "GitLab" };
  const EXECUTORS = {
    terminal: "Термінал у Mako (можна втрутитись)",
    "claude-headless": "Claude Code у фоні",
    orca: "Orca",
    command: "Своя команда",
  };
  const DEFAULT_LABELS = { trigger: "claude-agent", inProgress: "agent:in-progress", done: "agent:done", failed: "agent:failed" };
  const CLAUDE_ARGS = [
    "--permission-mode", "acceptEdits",
    "--allowedTools", "Bash(git:*) Bash(npm:*) Bash(npx:*) Bash(node:*) Read Edit Write Glob Grep",
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
    const { label, path, kind = "text", type = "string", placeholder = "", hint = "", options, wide, refresh, rerender } = o;
    const value = getAt(model, path);
    const attrs = `data-path="${h(path)}" data-type="${type}"${refresh ? " data-refresh" : ""}${rerender ? " data-rerender" : ""}`;
    let control;
    if (kind === "check") {
      const checked = type === "booltrue" ? value !== false : Boolean(value);
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
      control = `<input type="${kind}" ${attrs} value="${h(display(value, type))}" placeholder="${h(placeholder)}" spellcheck="false">`;
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

  function render() {
    const banner = !meta.configured
      ? `<div class="notice">👋 Mako ще не налаштований. Додайте трекер, вкажіть репозиторій і натисніть «Зберегти», після цього Mako одразу почне працювати.</div>`
      : meta.problem
        ? `<div class="notice err">Mako зараз не працює: ${h(meta.problem)}</div>`
        : "";
    root.innerHTML = `<h1>Налаштування</h1>${banner}
      ${trackersSection()}
      ${executorsSection()}
      ${secretsSection()}
      ${generalSection()}
      <p class="muted">Файл конфігу: <code>${h(meta.configPath || "")}</code></p>
      ${saveBar()}`;
  }

  function trackersSection() {
    const cards = model.trackers.map(trackerCard).join("");
    return `<section class="card"><div class="head"><h2>Трекери задач</h2></div>
      <p class="muted" style="margin-top:-6px">Звідки Mako бере задачі і в які локальні репозиторії їх віддає.</p>
      ${cards}
      <div class="adds" style="margin-top:14px"><span class="muted">Додати:</span>
        ${button("+ Jira", "add-tracker", { type: "jira" })}${button("+ GitHub", "add-tracker", { type: "github" })}${button("+ GitLab", "add-tracker", { type: "gitlab" })}</div>
    </section>`;
  }

  function trackerCard(t, ti) {
    const p = `trackers.${ti}.conf`;
    const type = t.conf.type;
    let fields = "";
    if (type === "jira") {
      fields = `
        ${field({ label: "JQL: які задачі віддавати агенту", path: `${p}.jql`, kind: "textarea", wide: true,
          placeholder: 'assignee = currentUser() AND labels = claude-agent AND statusCategory = "To Do"' })}
        ${field({ label: "Статус на старті", path: `${p}.transitions.onStart`, placeholder: "In Progress", hint: "Порожньо означає не змінювати" })}
        ${field({ label: "Статус після успіху", path: `${p}.transitions.onSuccess`, placeholder: "In Review" })}
        ${field({ label: "Статус при помилці", path: `${p}.transitions.onFailure`, placeholder: "не змінювати" })}
        ${field({ label: "Адреса Jira", path: `${p}.baseUrl`, placeholder: "з секрету JIRA_BASE_URL", hint: "Лише якщо відрізняється від секрету" })}`;
    } else {
      fields = `
        ${field({ label: "Мітка для агента", path: `${p}.labels.trigger`, placeholder: DEFAULT_LABELS.trigger, hint: "Issues з цією міткою потрапляють до Mako" })}
        ${field({ label: "Лише призначені на", path: `${p}.assignee`, placeholder: "будь-хто", hint: type === "github" ? "GitHub login" : "GitLab username" })}
        ${field({ label: "Мітка «в роботі»", path: `${p}.labels.inProgress`, placeholder: DEFAULT_LABELS.inProgress })}
        ${field({ label: "Мітка «готово»", path: `${p}.labels.done`, placeholder: DEFAULT_LABELS.done })}
        ${field({ label: "Мітка «помилка»", path: `${p}.labels.failed`, placeholder: DEFAULT_LABELS.failed })}
        ${type === "github"
          ? field({ label: "API (для Enterprise)", path: `${p}.apiUrl`, placeholder: "https://api.github.com" })
          : field({ label: "Адреса GitLab", path: `${p}.baseUrl`, placeholder: "https://gitlab.com" })}`;
    }
    const test = tests[ti];
    const testHtml = !test
      ? ""
      : test.loading
        ? `<div class="result muted">Перевіряю…</div>`
        : test.error
          ? `<div class="result err">✗ ${h(test.error)}</div>`
          : `<div class="result"><span style="color:var(--ok)">✓ Підключено.</span> Знайдено задач: <b>${test.result.count}</b>${
              test.result.sample.length
                ? `<ul>${test.result.sample.map((s) => `<li><code>${h(s.id)}</code> ${h(s.title)}</li>`).join("")}</ul>`
                : ""
            }</div>`;
    return `<div class="card"><div class="head"><h2>${TRACKERS[type]}</h2>
        <div>${button("Перевірити підключення", "test-tracker", { ti })}${button("Видалити", "remove-tracker", { ti }, "danger")}</div></div>
      <div class="grid">${fields}</div>${testHtml}
      <div style="margin-top:16px"><h3>Проєкти → репозиторії</h3></div>
      ${t.projects.map((proj, pi) => projectCard(t, ti, proj, pi)).join("")}
      <div class="adds" style="margin-top:12px">${button("+ проєкт", "add-project", { ti })}</div>
    </div>`;
  }

  function projectCard(t, ti, proj, pi) {
    const type = t.conf.type;
    const p = `trackers.${ti}.projects.${pi}`;
    const keyLabel = type === "jira" ? "Ключ проєкту Jira" : type === "github" ? "Репозиторій GitHub" : "Проєкт GitLab";
    const keyPlaceholder = type === "jira" ? "PROJ" : type === "github" ? "my-org/my-repo" : "group/project";
    const multi = proj.repos.length > 1;
    return `<div class="card"><div class="head"><div class="grid" style="flex:1">
        ${field({ label: keyLabel, path: `${p}.key`, placeholder: keyPlaceholder })}
        ${multi
          ? field({ label: "Репозиторій за замовчуванням", path: `${p}.rest.default`, kind: "select",
              options: [["", "немає (лише мітки й правила)"], ...proj.repos.map((r) => [repoName(r, proj), repoName(r, proj)])],
              hint: "Куди йдуть задачі без мітки <code>repo:</code> і без збігу правил" })
          : ""}
      </div>${button("Видалити", "remove-project", { ti, pi }, "link danger")}</div>
      ${proj.repos.map((r, ri) => repoCard(t, ti, proj, pi, r, ri)).join("")}
      <div class="adds" style="margin-top:10px">${button("+ ще один репозиторій", "add-repo", { ti, pi }, "link")}
        ${multi ? "" : `<span class="muted" style="font-size:13px">якщо задачі проєкту йдуть у різні репозиторії</span>`}</div>
    </div>`;
  }

  function repoCard(t, ti, proj, pi, r, ri) {
    const p = `trackers.${ti}.projects.${pi}.repos.${ri}`;
    const multi = proj.repos.length > 1;
    const defaultExec = model.general.defaultExecutor || model.executors[0]?.name || "";
    return `<div class="card"><div class="head"><strong>${h(repoName(r, proj))}</strong>
        ${multi ? button("Видалити", "remove-repo", { ti, pi, ri }, "link danger") : ""}</div>
      <div class="grid">
        ${field({ label: "Шлях до локального репозиторію", path: `${p}.conf.repoPath`, wide: true, placeholder: "/Users/me/code/my-repo", refresh: true })}
        ${field({ label: "Назва", path: `${p}.name`, placeholder: repoName({ ...r, name: "" }, proj), refresh: true,
          hint: multi ? `Мітка <code>repo:${h(repoName(r, proj))}</code> направляє задачу сюди` : "" })}
        ${field({ label: "Базова гілка", path: `${p}.conf.baseBranch`, placeholder: "main" })}
        ${field({ label: "Хто виконує", path: `${p}.conf.executor`, kind: "select",
          options: [["", `за замовчуванням (${defaultExec})`], ...model.executors.map((e) => [e.name, e.name])] })}
        ${field({ label: "Pull request", path: `${p}.conf.pullRequest`, kind: "select",
          options: [["", "автоматично (за origin)"], ["github", "GitHub (gh)"], ["gitlab", "GitLab (MR)"], ["none", "лише push"]] })}
        ${multi ? field({ label: "Правило: мітки", path: `${p}.conf.match.labels`, type: "csv", placeholder: "ui, frontend" }) : ""}
        ${multi && t.conf.type === "jira" ? field({ label: "Правило: компоненти Jira", path: `${p}.conf.match.components`, type: "csv", placeholder: "Frontend" }) : ""}
        ${field({ label: "Додаткові інструкції агенту", path: `${p}.conf.extraInstructions`, wide: true, placeholder: "Run `npm test` before committing." })}
      </div></div>`;
  }

  function executorsSection() {
    const defaultExec = model.general.defaultExecutor || model.executors[0]?.name;
    const cards = model.executors
      .map((e, ei) => {
        const p = `executors.${ei}.conf`;
        const type = e.conf.type;
        let fields = "";
        if (type === "terminal") {
          fields = `
            ${field({ label: "Команда агента", path: `${p}.command`, placeholder: "claude", hint: "На Windows: <code>claude.cmd</code>" })}
            ${field({ label: "Аргументи (по одному на рядок)", path: `${p}.args`, type: "lines", kind: "textarea", wide: true,
              placeholder: "{prompt}", hint: "За замовчуванням лише <code>{prompt}</code>. Сесія видна у вкладці «Вхідні» → «Термінал»" })}
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
            ${field({ label: "Команда", path: `${p}.command`, placeholder: "orca" })}
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
        return `<div class="card"><div class="head">
            <label class="check"><input type="radio" name="defexec" data-path="general.defaultExecutor" value="${h(e.name)}"${
              e.name === defaultExec ? " checked" : ""} data-rerender> за замовчуванням</label>
            ${model.executors.length > 1 ? button("Видалити", "remove-executor", { ei }, "link danger") : ""}</div>
          <div class="grid">
            ${field({ label: "Назва", path: `executors.${ei}.name`, refresh: true })}
            ${field({ label: "Тип", path: `${p}.type`, kind: "select", rerender: true, options: Object.entries(EXECUTORS) })}
            ${fields}
          </div></div>`;
      })
      .join("");
    return `<section class="card"><div class="head"><h2>Хто виконує задачі</h2></div>
      <p class="muted" style="margin-top:-6px">Mako не привʼязаний до інструмента: кожен репозиторій може мати свого виконавця.</p>
      ${cards}
      <div class="adds" style="margin-top:14px"><span class="muted">Додати:</span>
        ${button("+ Термінал", "add-executor", { type: "terminal" })}${button("+ Claude Code у фоні", "add-executor", { type: "claude-headless" })}${button("+ Orca", "add-executor", { type: "orca" })}${button("+ Своя команда", "add-executor", { type: "command" })}</div>
    </section>`;
  }

  function secretsSection() {
    const types = new Set(model.trackers.map((t) => t.conf.type));
    const needed = [
      ...(types.has("jira") ? [
        ["JIRA_BASE_URL", "Адреса Jira", "https://company.atlassian.net"],
        ["JIRA_EMAIL", "Email в Atlassian", "you@company.com"],
        ["JIRA_API_TOKEN", "API-токен Jira", "", 'Створити: <a href="https://id.atlassian.com/manage-profile/security/api-tokens" target="_blank" rel="noreferrer">id.atlassian.com</a>'],
      ] : []),
      ...(types.has("github") ? [["GITHUB_TOKEN", "GitHub token", "", "Необовʼязково, якщо виконано <code>gh auth login</code>"]] : []),
      ...(types.has("gitlab") ? [["GITLAB_TOKEN", "GitLab token", "", "Personal access token зі scope <code>api</code>"]] : []),
    ];
    if (!needed.length) return "";
    const rows = needed
      .map(([name, label, placeholder, hint]) => {
        const s = secrets[name] || {};
        const hidden = name.endsWith("_TOKEN");
        const edited = name in secretEdits;
        const value = edited ? secretEdits[name] ?? "" : hidden ? "" : s.value ?? "";
        const ph = hidden && s.set && !edited ? "•••••••• збережено" : placeholder;
        const state = edited && secretEdits[name] === null ? `<span class="hint err">буде видалено</span>` : "";
        return `<label class="f">${h(label)} <code style="font-size:11px">${name}</code>
          <span class="secret"><input type="${hidden ? "password" : "text"}" data-secret="${name}" value="${h(value)}" placeholder="${h(ph)}" autocomplete="off">
          ${s.set || edited ? button("×", "clear-secret", { name }, "link") : ""}</span>
          ${hint ? `<span class="hint">${hint}</span>` : ""}${state}</label>`;
      })
      .join("");
    return `<section class="card"><div class="head"><h2>Секрети</h2></div>
      <p class="muted" style="margin-top:-6px">Зберігаються лише локально у файлі <code>.env</code>. Токени не показуються після збереження.</p>
      <div class="grid">${rows}</div></section>`;
  }

  function generalSection() {
    return `<section class="card"><div class="head"><h2>Загальне</h2></div>
      <div class="grid">
        ${field({ label: "Нові задачі", path: "general.notifications.autoTake", kind: "select", type: "string",
          options: [["", "питати сповіщенням"], ["true", "брати без питань"]] })}
        ${field({ label: "Сповіщення на десктопі", path: "general.notifications.enabled", kind: "check", type: "booltrue" })}
        ${field({ label: "«Відкласти» на, хв", path: "general.notifications.snoozeMin", kind: "number", type: "number", placeholder: "60" })}
        ${field({ label: "Ліміт задач у роботі (WIP)", path: "general.wipLimit", kind: "number", type: "number", placeholder: "без ліміту",
          hint: "Як у канбані: поки стільки задач взято й не завершено, Mako не опитує трекер. 0 або порожньо: без ліміту" })}
        ${field({ label: "Агентів одночасно", path: "general.maxConcurrent", kind: "number", type: "number",
          placeholder: String(model.general.wipLimit || 1), hint: "За замовчуванням дорівнює WIP-ліміту" })}
        ${field({ label: "Перевіряти трекери кожні, с", path: "general.pollIntervalSec", kind: "number", type: "number", placeholder: "60" })}
        ${field({ label: "Ліміт на задачу, хв", path: "general.taskTimeoutMin", kind: "number", type: "number", placeholder: "45" })}
        ${field({ label: "Шукати PR переданих задач кожні, хв", path: "general.handoffCheckMin", kind: "number", type: "number", placeholder: "5", hint: "0 вимикає" })}
        ${field({ label: "Порт цієї сторінки", path: "general.notifications.inboxPort", kind: "number", type: "number", placeholder: "7777", hint: "Діє після перезапуску Mako" })}
      </div></section>`;
  }

  function saveBar() {
    const msg = status ? `<span class="msg ${status.kind === "err" ? "err" : ""}"${status.kind === "ok" ? ' style="color:var(--ok)"' : ""}>${status.text}</span>` : `<span class="msg muted">${dirty ? "Є незбережені зміни" : ""}</span>`;
    return `<div class="savebar"><div class="in">${msg}
      ${dirty ? button("Скасувати", "reset") : ""}
      ${button(saving ? "Зберігаю…" : "Зберегти", "save", {}, "primary")}</div></div>`;
  }

  // ---------- actions ----------

  const actions = {
    "add-tracker"({ type }) {
      const conf = type === "jira"
        ? { type, jql: 'assignee = currentUser() AND labels = claude-agent AND statusCategory = "To Do"', transitions: { onStart: "In Progress", onSuccess: "In Review" } }
        : { type };
      model.trackers.push({ conf, projects: [{ key: "", rest: {}, repos: [{ name: "", conf: {} }] }] });
    },
    "remove-tracker"({ ti }) {
      if (confirm("Видалити цей трекер?")) model.trackers.splice(ti, 1), (tests = {});
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
    },
    "remove-executor"({ ei }) {
      const [removed] = model.executors.splice(ei, 1);
      if (model.general.defaultExecutor === removed.name) delete model.general.defaultExecutor;
      for (const t of model.trackers) for (const p of t.projects) for (const r of p.repos) if (r.conf.executor === removed.name) delete r.conf.executor;
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

  root.addEventListener("change", (e) => {
    if (e.target.dataset.refresh !== undefined) render();
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
