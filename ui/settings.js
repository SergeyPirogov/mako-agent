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
      const tidied = tidyExecutorNames();
      if (tidied.length) {
        dirty = true;
        status = { kind: "warn", text: `Виконавців перейменовано за типом: ${h(tidied.join(", "))}. Збережіть, щоб застосувати.` };
      }
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

  /** Where Mako keeps a repo without a path of its own: <projects folder>/<name>, as the server resolves it. */
  function defaultRepoPath(r, p) {
    const dir = (model.general.projectsDir || "~/mako/projects").replace(/[\\/]+$/, "");
    return `${dir}/${r.name.trim() || p.key.split("/").pop() || "repo"}`;
  }

  /** The SSH address Mako clones a GitLab/GitHub project from, unless «Звідки клонувати» says otherwise. */
  function defaultCloneUrl(t, p) {
    const key = p.key.trim();
    if (!key.includes("/") || p.repos.length > 1) return "";
    try {
      if (t.conf.type === "gitlab") return `git@${new URL(t.conf.baseUrl || "https://gitlab.com").hostname}:${key}.git`;
      if (t.conf.type === "github") return `git@${t.conf.apiUrl ? new URL(t.conf.apiUrl).hostname.replace(/^api\./, "") : "github.com"}:${key}.git`;
    } catch {}
    return "";
  }

  function repoName(r, p) {
    const fromPath = (r.conf.repoPath || "").replace(/[\\/]+$/, "").split(/[\\/]/).pop();
    return r.name.trim() || fromPath || p.key.split("/").pop() || "repo";
  }

  /** Drops empty values so Mako's defaults apply. */
  function clean(v, parent = "") {
    if (Array.isArray(v)) return v.map((x) => clean(x));
    if (v && typeof v === "object") {
      const out = {};
      for (const [k, x] of Object.entries(v)) {
        const c = clean(x, k);
        if (c === undefined || c === "") continue;
        // A project or repo with nothing set is still there: everything about it is the default.
        const keep = k === "projects" || k === "repos" || parent === "projects" || parent === "repos";
        if (c && typeof c === "object" && !Array.isArray(c) && !Object.keys(c).length && !keep) continue;
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
      if (!t.projects.length && !t.conf.allProjects) errs.push(`${name}: додайте проєкт або увімкніть «Мої задачі з усіх проєктів»`);
      dup(t.projects.map((p) => p.key.trim()), `${name}, проєкти`);
      t.projects.forEach((p) => {
        p.repos.forEach((r) => {
          // Without a path Mako clones into the projects folder, which needs an address to clone from.
          if (!r.conf.repoPath && !r.conf.cloneUrl && !defaultCloneUrl(t, p)) {
            errs.push(`${name}, ${p.key || "проєкт"}: вкажіть шлях до репозиторію або «Звідки клонувати»`);
          }
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

  /** As Mako resolves it: the chosen one, else the Mako terminal, else the first. */
  function defaultExecutorName() {
    return model.general.defaultExecutor || model.executors.find((e) => e.conf.type === "terminal")?.name || model.executors[0]?.name || "";
  }

  const BASE_NAMES = { terminal: "terminal", "claude-headless": "claude", orca: "orca", command: "command" };

  /** Renames an executor and everything that points at it: the default and the repos that chose it. */
  function renameExecutor(from, to) {
    if (!from || from === to) return;
    if (model.general.defaultExecutor === from) model.general.defaultExecutor = to;
    for (const t of model.trackers) for (const p of t.projects) for (const r of p.repos) if (r.conf.executor === from) r.conf.executor = to;
  }

  /**
   * Names like "terminal" on an Orca executor, or "terminal-2" next to it, come from changing a type after adding:
   * give each generated name its type's plain name ("orca", "terminal"), so the list reads as what it is.
   */
  function tidyExecutorNames() {
    const changes = [];
    const generated = (e) => /^(terminal|claude|orca|command)(-\d+)?$/.test(e.name);
    for (let pass = 0; pass < 2; pass++) {
      for (const e of model.executors) {
        const base = BASE_NAMES[e.conf.type];
        if (!base || !generated(e) || e.name === base) continue;
        if (e.name.replace(/-\d+$/, "") === base && model.executors.some((x) => x.name === base)) continue;
        if (model.executors.some((x) => x !== e && x.name === base)) continue; // taken: next pass, if it gets freed
        changes.push(`${e.name} → ${base}`);
        renameExecutor(e.name, base);
        e.name = base;
      }
    }
    return changes;
  }

  // ---------- branch names: the same rules as src/git.ts ----------

  const DEFAULT_BRANCH = "{type}/{id}-{title}";
  const BRANCH_HINT = "<code>{type}</code>: fix для багів, інакше feat · <code>{id}</code>: номер задачі · <code>{title}</code>: назва латиницею · <code>{slug}</code>: репозиторій-номер.";
  const BRANCH_LIST = `<datalist id="branch-patterns">${["{type}/{id}-{title}", "feature/{id}-{title}", "feat/{id}-{title}", "fix/{id}-{title}", "{type}/{id}", "agent/{slug}"]
    .map((x) => `<option value="${x}">`).join("")}</datalist>`;
  const TRANSLIT = { а: "a", б: "b", в: "v", г: "h", ґ: "g", д: "d", е: "e", є: "ye", ж: "zh", з: "z", и: "y", і: "i", ї: "yi", й: "i", к: "k", л: "l", м: "m", н: "n", о: "o", п: "p", р: "r", с: "s", т: "t", у: "u", ф: "f", х: "kh", ц: "ts", ч: "ch", ш: "sh", щ: "shch", ь: "", ю: "yu", я: "ya" };
  function branchExample(pattern) {
    const title = "Розібрати результати нічного рану тестів"
      .toLowerCase().replace(/[а-яіїєґ]/g, (c) => TRANSLIT[c] ?? "").replace(/[^a-z0-9]+/g, " ").trim().split(/\s+/)
      .reduce((acc, w) => (acc.full || (acc.out && acc.out.length + 1 + w.length > 40) ? { ...acc, full: true } : { out: acc.out ? `${acc.out}-${w}` : w }), { out: "" }).out;
    return (pattern || DEFAULT_BRANCH).replace(/\{type\}/g, "feat").replace(/\{id\}/g, "17").replace(/\{title\}/g, title).replace(/\{slug\}/g, "market-autotests-17");
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
      ["s-templates", "Шаблони й памʼять", templatesSection()],
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
      // Statuses of the project's workflows, once «Перевірити підключення» has read them.
      const found = tests[ti]?.result?.statuses;
      const jiraStatuses = {
        list: found ? `<datalist id="statuses-${ti}">${found.map((x) => `<option value="${h(x)}">`).join("")}</datalist>` : "",
        hint: found ? `Статуси проєкту: ${found.map((x) => `<code>${h(x)}</code>`).join(", ")}` : "Назва статусу або переходу Jira, напр. <code>In Progress</code>. Список зʼявиться після «Перевірити підключення»",
      };
      main = `
        ${secretField("JIRA_BASE_URL", "Адреса Jira", "https://company.atlassian.net")}
        ${secretField("JIRA_EMAIL", "Email в Atlassian", "you@company.com")}
        ${secretField("JIRA_API_TOKEN", "API-токен", "", 'Створити: <a href="https://id.atlassian.com/manage-profile/security/api-tokens" target="_blank" rel="noreferrer">id.atlassian.com → API tokens</a>')}
        ${field({ label: "Які задачі віддавати агенту (JQL)", path: `${p}.jql`, kind: "textarea", wide: true,
          placeholder: 'assignee = currentUser() AND labels = claude-agent AND statusCategory = "To Do"',
          hint: "Порядок задає <code>ORDER BY</code>: пріоритетні задачі пропонуються першими" })}
        ${jiraStatuses.list}
        ${field({ label: "Статус, коли агент почав", path: `${p}.transitions.onStart`, placeholder: "не змінювати", list: `statuses-${ti}`, hint: jiraStatuses.hint })}
        ${field({ label: "Статус, коли є MR", path: `${p}.transitions.onSuccess`, placeholder: "не змінювати", list: `statuses-${ti}` })}`;
      advanced = `
        ${field({ label: "Статус при помилці", path: `${p}.transitions.onFailure`, placeholder: "не змінювати", list: `statuses-${ti}` })}
        ${field({ label: "Статус, коли MR змерджено", path: `${p}.transitions.onMerged`, placeholder: "автоматично: Done", list: `statuses-${ti}` })}
        ${field({ label: "«Взяти» призначає задачу на мене", path: `${p}.assignOnTake`, kind: "check", type: "booltrue", wide: true })}
        ${field({ label: "Не пропонувати задачі, призначені на інших (без мене серед виконавців)", path: `${p}.skipOthersTasks`, kind: "check", type: "booltrue", wide: true })}
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
        ${field({ label: `Мої задачі з усіх проєктів: проєкт задачі Mako визначить сам, знайде ваш клон або склонує в ${h(model.general.projectsDir || "~/mako/projects")}`, path: `${p}.allProjects`, kind: "check", type: "bool", wide: true, rerender: true,
          hint: "Тоді нижче потрібні лише проєкти зі своїми налаштуваннями (шлях, гілка, виконавець)" })}
        ${field({ label: "«Взяти» призначає задачу на мене (колеги бачать, що її взято, і Mako їм її не пропонує)", path: `${p}.assignOnTake`, kind: "check", type: "booltrue", wide: true })}
        ${field({ label: "Не пропонувати задачі, призначені на інших (зніміть, якщо на задачі завжди призначають кількох, напр. розробника й QA)", path: `${p}.skipOthersTasks`, kind: "check", type: "booltrue", wide: true })}
        ${isLab ? `${statusList}
          ${field({ label: "Статус, коли агент почав", path: `${p}.transitions.onStart`, placeholder: isLab ? "автоматично: In progress" : "не змінювати", list: `statuses-${ti}`, hint: statusHint })}
          ${field({ label: "Статус, коли є MR", path: `${p}.transitions.onSuccess`, placeholder: "не змінювати", list: `statuses-${ti}`, refresh: true,
            hint: /^(done|closed|completed|готово|виконано)$/i.test(t.conf.transitions?.onSuccess || "")
              ? `<span class="warn">⚠ «${h(t.conf.transitions.onSuccess)}» ставиться ще до мерджу, коли MR тільки відкрито. Mako сам переведе задачу в Done після мерджу; тут краще статус ревʼю або порожньо</span>`
              : "Після мерджу MR задача сама перейде в Done і закриється" })}` : ""}`;
      advanced = `
        ${isLab ? field({ label: "Статус при помилці", path: `${p}.transitions.onFailure`, placeholder: "не змінювати", list: `statuses-${ti}` }) : ""}
        ${isLab ? field({ label: "Статус, коли MR змерджено", path: `${p}.transitions.onMerged`, placeholder: "автоматично: Done", list: `statuses-${ti}`, hint: "Mako також закриє задачу, якщо GitLab ще не закрив" }) : ""}
        ${field({ label: "Мітка «в роботі» (необовʼязково)", path: `${p}.labels.inProgress`, placeholder: "не ставити" })}
        ${field({ label: "Мітка «готово» (необовʼязково)", path: `${p}.labels.done`, placeholder: "не ставити" })}
        ${field({ label: "Мітка «помилка» (необовʼязково)", path: `${p}.labels.failed`, placeholder: "не ставити" })}
        ${isLab ? "" : field({ label: "API (для GitHub Enterprise)", path: `${p}.apiUrl`, placeholder: "https://api.github.com", refresh: true })}`;
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
      <datalist id="projects-${ti}">${projectOptions(ti)}</datalist>
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
        ${field({ label: keyLabel, path: `${p}.key`, placeholder: keyPlaceholder, refresh: true, list: `projects-${ti}`,
          hint: `<span class="proj-hint" data-ti="${ti}">${projectHint(ti)}</span>` })}
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
    const defaultExec = defaultExecutorName();
    const fallback = defaultRepoPath(r, proj);

    const clone = r.conf.cloneUrl || defaultCloneUrl(t, proj);
    // A new project without its name yet: nothing to check, the default folder would be «…/repo».
    const unnamed = !r.conf.repoPath && !r.name.trim() && !proj.key.trim();
    const check = unnamed ? "" : pathCheckKey(r.conf.repoPath || fallback, r.conf.baseBranch, clone, ti, pi, ri);
    return `<div class="card repo"><div class="head"><strong>📁 ${h(repoName(r, proj))}</strong>
        ${multi ? button("Видалити", "remove-repo", { ti, pi, ri }, "link danger") : ""}</div>
      <div class="grid">
        ${field({ label: "Шлях до локального репозиторію", path: `${p}.conf.repoPath`, wide: true, placeholder: `${fallback} (за замовчуванням)`, refresh: true,
          hint: unnamed
            ? `<span class="muted">Спершу вкажіть ${t.conf.type === "jira" ? "ключ проєкту" : "проєкт"} вище: тоді буде видно, куди Mako склонує код</span>`
            : `<span class="pathcheck" data-check="${h(check)}">${pathCheckHtml(check)}</span>` })}
        ${field({ label: "Базова гілка", path: `${p}.conf.baseBranch`, placeholder: "автоматично", refresh: true, hint: "Порожньо: основна гілка репозиторію (origin/HEAD)" })}
        ${field({ label: "Хто виконує", path: `${p}.conf.executor`, kind: "select",
          options: [["", `основний (${defaultExec})`], ...model.executors.map((e) => [e.name, `${e.name} · ${EXECUTORS[e.conf.type] ?? e.conf.type}`])] })}
        ${multi ? field({ label: "Сюди йдуть задачі з мітками", path: `${p}.conf.match.labels`, type: "csv", placeholder: "ui, frontend" }) : ""}
        ${multi && t.conf.type === "jira" ? field({ label: "…або з компонентами Jira", path: `${p}.conf.match.components`, type: "csv", placeholder: "Frontend" }) : ""}
      </div>
      ${details(`repo-${ti}-${pi}-${ri}`, "Додатково", `
        ${field({ label: "Звідки клонувати", path: `${p}.conf.cloneUrl`, wide: true, placeholder: defaultCloneUrl(t, proj) || "git@gitlab.com:group/repo.git", refresh: true,
          hint: "Якщо в папці репозиторію ще немає клону, Mako склонує його звідси перед першою задачею. Потрібен SSH-ключ або збережений пароль git" })}
        ${field({ label: "Підготовка worktree", path: `${p}.conf.setup`, wide: true, placeholder: "auto: за lock-файлом (pnpm install, npm ci, …)",
          hint: "Команда в новому worktree до старту агента, щоб працювали тести, лінтери й git-хуки. <code>none</code>: нічого не запускати" })}
        ${field({ label: "Назва гілки задачі", path: `${p}.conf.branchPattern`, wide: true, placeholder: `як загальна: ${model.general.branchPattern || DEFAULT_BRANCH}`, list: "branch-patterns", rerender: true,
          hint: `Лише для цього репозиторію. ${r.conf.branchPattern ? `Наприклад: <code>${h(branchExample(r.conf.branchPattern))}</code>` : ""}` })}
        ${field({ label: "Додаткові інструкції агенту", path: `${p}.conf.extraInstructions`, wide: true, placeholder: "Run `pnpm lint` before committing." })}
        ${field({ label: "Pull request", path: `${p}.conf.pullRequest`, kind: "select",
          options: [["", "автоматично (за origin)"], ["github", "GitHub (gh)"], ["gitlab", "GitLab (MR)"], ["none", "лише push"]] })}
        ${field({ label: "Назва", path: `${p}.name`, placeholder: repoName({ ...r, name: "" }, proj), refresh: true,
          hint: multi ? `Мітка <code>repo:${h(repoName(r, proj))}</code> направляє задачу сюди` : "Для мітки <code>repo:назва</code>" })}
        ${field({ label: "Папка для worktree", path: `${p}.conf.worktreesDir`, wide: true, placeholder: "поруч із репозиторієм: ../.worktrees/<назва>" })}`)}
    </div>`;
  }

  // ---- projects of a tracker, offered in the project field ----

  const projectLists = new Map(); // ti -> { sig, state: "loading" | "ok" | "error", list, error }

  /** What the list depends on: the tracker's address and the token typed but not saved yet. */
  function projectsSig(ti) {
    const c = model.trackers[ti].conf;
    return JSON.stringify([c.type, c.baseUrl, c.apiUrl, secretEdits]);
  }

  function projectOptions(ti) {
    const e = projectLists.get(ti);
    if (e?.sig !== projectsSig(ti) || e.state !== "ok") return "";
    return e.list.map((p) => `<option value="${h(p.key)}">${h(p.name)}</option>`).join("");
  }

  function projectHint(ti) {
    const e = projectLists.get(ti);
    if (!e || e.sig !== projectsSig(ti)) return "Почніть вводити: Mako підкаже проєкти з трекера";
    if (e.state === "loading") return "Завантажую проєкти…";
    if (e.state === "error") return `<span class="warn">Список проєктів не завантажився: ${h(e.error)}</span>`;
    return e.list.length ? `Проєктів у списку: ${e.list.length}, почніть вводити назву` : "Трекер не повернув жодного проєкту";
  }

  async function loadProjects(ti) {
    const sig = projectsSig(ti);
    if (projectLists.get(ti)?.sig === sig) return;
    projectLists.set(ti, { sig, state: "loading" });
    const redraw = () => {
      const list = root.querySelector(`#projects-${ti}`);
      if (list) list.innerHTML = projectOptions(ti);
      root.querySelectorAll(`.proj-hint[data-ti="${ti}"]`).forEach((n) => (n.innerHTML = projectHint(ti)));
    };
    redraw();
    try {
      const raw = toRaw({ general: {}, executors: [], trackers: [model.trackers[ti]] });
      const { projects } = await api("POST", "/api/tracker-projects", { tracker: raw.trackers[0], secrets: secretEdits });
      projectLists.set(ti, { sig, state: "ok", list: projects });
    } catch (err) {
      projectLists.set(ti, { sig, state: "error", error: err.message });
    }
    redraw();
  }

  root.addEventListener("focusin", (e) => {
    const path = e.target.dataset?.path;
    const m = path && path.match(/^trackers\.(\d+)\.projects\.\d+\.key$/);
    if (m) loadProjects(Number(m[1]));
  });

  // ---- live check of repository paths ----

  // key: "path|base|clone|ti.pi.ri"; base and clone may be empty
  function pathCheckKey(repoPath, base, clone = "", ti = "", pi = "", ri = "") {
    return repoPath ? `${repoPath}|${base || ""}|${clone || ""}|${ti}.${pi}.${ri}` : "";
  }

  function pathCheckHtml(key) {
    if (!key) return "";
    const c = pathChecks.get(key);
    if (!c || c === "pending") return `<span class="muted">перевіряю шлях…</span>`;
    if (c === "cloning") return `<span class="muted">клоную…</span>`;
    if (c.error) return c.message ? `<span class="err">✗ ${h(c.message)}</span>` : "";
    const [, setBase, clone, at] = key.split("|");
    const base = setBase || c.defaultBranch;
    if (!c.exists || (c.empty && !c.git)) {
      if (!clone) return `<span class="err">✗ Папки не існує: ${h(c.resolved)}. Вкажіть шлях до клону або «Звідки клонувати» в «Додатково»</span>`;
      const [ti, pi, ri] = at.split(".");
      return `<span class="muted">Клону ще немає: Mako склонує <code>${h(clone)}</code> перед першою задачею.</span> ${button("Склонувати зараз", "clone-repo", { key, ti, pi, ri }, "link")}`;
    }
    if (!c.git) return `<span class="warn">⚠ Це не git-репозиторій</span>`;
    if (!base) return `<span class="warn">⚠ Не вдалось визначити основну гілку: вкажіть «Базова гілка»</span>`;
    if (!c.hasBase) return `<span class="warn">⚠ Гілки <code>origin/${h(base)}</code> не знайдено: перевірте назву або зробіть <code>git fetch</code></span>`;
    return `<span class="ok-text">✓ git-репозиторій, гілка <code>origin/${h(base)}</code>${setBase ? "" : " (визначено автоматично)"}</span>`;
  }

  function redrawCheck(key) {
    root.querySelectorAll(`[data-check="${CSS.escape(key)}"]`).forEach((n) => (n.innerHTML = pathCheckHtml(key)));
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
        .finally(() => redrawCheck(key));
    }
  }

  function executorsSection() {
    const defaultExec = defaultExecutorName();
    const usage = (name) =>
      model.trackers.flatMap((t) => t.projects.flatMap((p) => p.repos)).filter((r) => (r.conf.executor || defaultExec) === name).length;
    // The default first: it is the one that runs the tasks.
    const order = model.executors.map((e, ei) => ei).sort((a, b) => (model.executors[b].name === defaultExec) - (model.executors[a].name === defaultExec));
    const rows = order
      .map((ei) => {
        const e = model.executors[ei];
        const p = `executors.${ei}.conf`;
        const type = e.conf.type;
        const open = openExecs.has(ei);
        let fields = "";
        if (type === "terminal") {
          fields = `
            ${field({ label: "Команда агента", path: `${p}.command`, placeholder: "claude", hint: "Повний шлях, якщо Mako не знаходить команду. На Windows: <code>claude.cmd</code>" })}
            ${field({ label: "Аргументи (по одному на рядок)", path: `${p}.args`, type: "lines", kind: "textarea", wide: true,
              placeholder: "{prompt}", hint: "За замовчуванням лише <code>{prompt}</code>" })}
            ${field({ label: "Що Claude робить без питань", path: `${p}.permissionMode`, kind: "select", type: "string", wide: true,
              options: [["", "auto mode: Claude сам вирішує, що безпечно, і питає лише про ризиковане (рекомендовано)"], ["acceptEdits", "редагує файли задачі, про команди питає"], ["default", "питає про все"], ["bypassPermissions", "нічого не питає (небезпечно: будь-які команди без вашого відома)"]],
              hint: "Працює в окремому worktree задачі; ваша робоча папка не зачіпається. Auto mode потрібна свіжа версія Claude Code" })}
            ${field({ label: "Команди без питань (по одній на рядок)", path: `${p}.allowedTools`, type: "lines", kind: "textarea", wide: true,
              placeholder: "Bash(git:*)\nBash(pnpm:*)\nBash(npx playwright:*)",
              hint: `Формат Claude Code: <code>Bash(команда:*)</code>, <code>WebFetch</code>, <code>mcp__…</code>. ${button("+ звичні: git, npm, pnpm, yarn, node, тести", "fill-tools", { ei }, "link")}` })}
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
              <span class="caret">${open ? "▾" : "▸"}</span><strong>${h(EXECUTORS[type] ?? type)}</strong>
              <code class="muted small" title="Назва в конфігу">${h(e.name || "без назви")}</code></button>
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
        ${field({ label: "Архівувати завершені через, днів", path: "general.archiveAfterDays", kind: "number", type: "number", placeholder: "2",
          hint: "Готові й скасовані задачі йдуть в архів самі. «Не виконано» і задачі з відкритим MR лишаються. 0: не архівувати" })}
        ${field({ label: "Спершу план: агент пропонує план, а працює після вашого погодження (для окремої задачі вимикається в «Взяти ▾»)", path: "general.planFirst", kind: "check", type: "booltrue", wide: true })}
        ${field({ label: "Назва гілки задачі", path: "general.branchPattern", wide: true, placeholder: DEFAULT_BRANCH, list: "branch-patterns", rerender: true,
          hint: `${BRANCH_HINT} Наприклад: <code>${h(branchExample(model.general.branchPattern))}</code>` })}
      </div></div>${BRANCH_LIST}
      <div class="shead" style="margin-top:24px"><h2>Автопілот MR</h2>
        <p class="muted">Mako сам віддає фоновому агенту впалий CI, нові коментарі рев’юверів і конфлікти, поки MR не стане готовим до мерджу. На картці його можна вмикати й вимикати для окремої задачі.</p></div>
      <div class="card"><div class="grid">
        ${field({ label: "Увімкнути автопілот для всіх MR", path: "general.autopilot.enabled", kind: "check", type: "bool", wide: true })}
        ${field({ label: "Не більше раундів на задачу", path: "general.autopilot.maxRounds", kind: "number", type: "number", placeholder: "3",
          hint: "Потім автопілот зупиняється й кличе вас" })}
        ${field({ label: "Мерджити самому, коли MR готовий (CI зелений, коментарів і конфліктів немає, апруви є)", path: "general.autopilot.merge", kind: "check", type: "bool", wide: true })}
      </div></div>`;
  }

  function templatesSection() {
    const list = model.general.templates || [];
    const rows = list
      .map((t, i) => {
        const p = `general.templates.${i}`;
        return `<div class="card tpl">
          <div class="head"><div class="title"><h3 class="tname">${h(t.name || "Новий шаблон")}</h3></div>
            <div class="tools">${button("Видалити", "remove-template", { i }, "link danger")}</div></div>
          <div class="grid">
            ${field({ label: "Назва", path: `${p}.name`, placeholder: "Розбір нічного рану", refresh: true })}
            ${field({ label: "Для задач з мітками", path: `${p}.labels`, type: "csv", placeholder: "nightly-run, triage" })}
            ${field({ label: "…або з назвою, що містить", path: `${p}.titleContains`, type: "csv", placeholder: "нічного рану, nightly" })}
            ${field({ label: "Інструкції агенту", path: `${p}.instructions`, kind: "textarea", wide: true,
              placeholder: "Завантаж звіт з …/report/failed.jsonl. Для кожного падіння визнач: флейк, баг продукту чи тесту. Флейки онови з ретраєм, на баги продукту заведи задачу в GitLab з міткою bug…",
              hint: "Додаються до промпта кожної задачі, що підходить за міткою чи назвою; у «+ Задача» шаблон можна вибрати явно" })}
          </div></div>`;
      })
      .join("");
    return `<div class="shead"><h2>Шаблони задач</h2>
        <p class="muted">Готові інструкції для задач, що повторюються: агент отримує їх разом із текстом задачі.</p></div>
      ${rows || `<p class="muted">Шаблонів ще немає.</p>`}
      <div class="adds">${button("+ Шаблон", "add-template")}</div>
      <div class="shead" style="margin-top:24px"><h2>Памʼять репозиторіїв</h2></div>
      <div class="card"><div class="grid">
        ${field({ label: "Збирати й передавати агентам те, що вони дізналися про репозиторій (як запускати тести, структура, підводні камені)", path: "general.memory", kind: "check", type: "booltrue", wide: true })}
      </div><p class="small"><a href="/memory">Переглянути й відредагувати памʼять →</a></p></div>`;
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
        ${field({ label: "Папка для проєктів", path: "general.projectsDir", placeholder: "~/mako/projects", refresh: true, rerender: true,
          hint: "Сюди Mako клонує репозиторії, для яких не вказано свого шляху" })}
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
    async "clone-repo"({ key }) {
      const [repoPath, base, url] = key.split("|");
      pathChecks.set(key, "cloning");
      redrawCheck(key);
      try {
        await api("POST", "/api/clone", { path: repoPath, url });
        pathChecks.set(key, await api("GET", `/api/check-path?path=${encodeURIComponent(repoPath)}&base=${encodeURIComponent(base)}`));
      } catch (err) {
        pathChecks.set(key, { error: true, message: err.message });
      }
      redrawCheck(key);
      return "skip";
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
    "add-template"() {
      model.general.templates = [...(model.general.templates || []), { name: "", labels: [], titleContains: [], instructions: "" }];
    },
    "remove-template"({ i }) {
      model.general.templates.splice(Number(i), 1);
      if (!model.general.templates.length) delete model.general.templates;
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
    "fill-tools"({ ei }) {
      const conf = model.executors[ei].conf;
      const common = ["Bash(git:*)", "Bash(npm:*)", "Bash(npx:*)", "Bash(pnpm:*)", "Bash(yarn:*)", "Bash(node:*)", "Bash(bun:*)", "Bash(ls:*)", "Bash(cat:*)", "Bash(grep:*)", "Bash(rg:*)"];
      conf.allowedTools = [...new Set([...(conf.allowedTools ?? []), ...common])];
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
      const old = getAt(model, el.dataset.path);
      setAt(model, el.dataset.path, value);
      if (/^executors\.\d+\.name$/.test(el.dataset.path)) renameExecutor(old, value);
      if (/^executors\.\d+\.conf\.type$/.test(el.dataset.path)) tidyExecutorNames();
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
