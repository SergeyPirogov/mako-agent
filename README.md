# 🦈 Mako

> Mako — найшвидша акула. Помічає нову задачу в трекері, питає, чи брати, і віддає її вашому агенту.

Локальний демон, який стежить за задачами в **Jira, GitHub Issues або GitLab Issues** і делегує їх
агенту на вашому компʼютері (за замовчуванням **Claude Code**). Коли зʼявляється нова задача, на десктопі
показується сповіщення **«🦈 У вас нова задача»** з кнопками **Взяти** і **Відкласти**.
Кожна взята задача виконується в окремому git worktree. Чим виконувати задачу, вирішуєте ви:
фоновий Claude Code, Orca, Codex, tmux або будь-яка інша команда. Mako не привʼязаний до конкретного інструмента.

```
 Jira / GitHub / GitLab ──poll──► orchestrator ──► 🔔 «У вас нова задача»  [Взяти] [Відкласти]
                                       │                      │ Взяти
                                       │         git worktree agent/<slug> + claude -p "<задача>"
                                       │                      │
     коментар + статус/мітка ◄─────────┴──── push + PR (GitHub) / MR (GitLab)
```

## Життєвий цикл задачі

| Стан | Що відбувається |
|---|---|
| **Нова** | Задачу знайдено, на екрані сповіщення з кнопками |
| **Відкладена** | Ви натиснули «Відкласти» або проігнорували сповіщення. Через `snoozeMin` хвилин спитає знову |
| **У черзі** | Ви натиснули «Взяти». Агент стартує, щойно звільниться слот (`maxConcurrent`) |
| **Виконується** | Задача → In Progress (Jira) або мітка `agent:in-progress` (GitHub/GitLab), запускається виконавець |
| **Передано** | Задачу відкрито в Orca, tmux тощо. Статус у трекері змінено на стартовий, далі керуєте ви |
| **Готово** | Push гілки, PR/MR, підсумок агента в коментарі, задача → In Review або мітка `agent:done`, сповіщення ✅ |
| **Помилка** | Коментар із причиною, мітка `agent:failed`, сповіщення ❌. На сторінці «Вхідні» є кнопка «Повторити» |

Якщо сповіщення пропущене, ті самі кнопки є на сторінці **«Вхідні»**: http://127.0.0.1:7777.
Стан зберігається в `.mako/state.json`. Після перезапуску відкладені задачі не губляться,
а перервані Ctrl+C знову стають у чергу. Повні логи агента лежать у `.mako/logs/`.

## Налаштування

Потрібні Node.js 20.12+, `claude` (з виконаним входом) і `git`. Для PR у GitHub потрібен `gh` з виконаним `gh auth login`.
MR у GitLab створюється через git push options, тож `glab` не потрібен.

```bash
npm install
cp .env.example .env                              # токени лише для тих трекерів, які використовуєте
cp mako.config.example.json mako.config.json
```

### Трекери

У `trackers` можна вказати один або кілька трекерів одночасно. Кожен має свій `projects`:
ключ проєкту в трекері → локальні репозиторії (див. нижче).

| `type` | Які задачі беруться | Як позначається прогрес | Ключ у `projects` | Секрети |
|---|---|---|---|---|
| `jira` | За `jql` | Статуси з `transitions` | Ключ проєкту: `PROJ` | `JIRA_BASE_URL`, `JIRA_EMAIL`, `JIRA_API_TOKEN` |
| `github` | Відкриті issues з міткою `labels.trigger` (+ `assignee`) | Мітки `inProgress` / `done` / `failed` | `owner/repo` | `GITHUB_TOKEN` або `gh auth login` |
| `gitlab` | Відкриті issues з міткою `labels.trigger` (+ `assignee`) | Ті самі мітки | `group/project` | `GITLAB_TOKEN` (scope `api`) |

Для GitHub Enterprise вкажіть `apiUrl`, для self-hosted GitLab вкажіть `baseUrl`.
У проєкті `pullRequest` може бути `auto` (за адресою `origin`), `github`, `gitlab` або `none`.
PR/MR містить `Closes owner/repo#N`, тож після мерджу issue закривається автоматично.

### У якому репозиторії виконується задача

Кожен проєкт трекера привʼязаний до одного або кількох локальних репозиторіїв.
Найпростіший випадок: один проєкт відповідає одному репозиторію.

```json
"projects": { "my-org/web": { "repoPath": "/Users/me/code/web", "baseBranch": "main" } }
```

Якщо в одному проєкті є задачі для різних репозиторіїв, опишіть їх у `repos`:

```json
"projects": {
  "PROJ": {
    "default": "backend",
    "repos": {
      "backend": { "repoPath": "/Users/me/code/backend", "match": { "components": ["Backend", "API"] } },
      "web":     { "repoPath": "/Users/me/code/web",     "match": { "components": ["Frontend"], "labels": ["ui"] } }
    }
  }
}
```

Репозиторій для задачі вибирається за пріоритетом:

1. **Мітка `repo:<назва>`** на задачі, наприклад `repo:web`. Працює в Jira, GitHub і GitLab.
2. **Правила `match`**: перший репозиторій, у якого збіглася мітка або компонент Jira. Регістр не враховується.
3. **`default`**, або єдиний репозиторій проєкту.

Якщо репозиторій визначити не вдалося (немає збігів і немає `default`, або `repo:` вказує на невідому назву),
задача не пропонується, а в лог пишеться причина. Вибраний репозиторій видно в сповіщенні
(`GitHub → web`), на сторінці «Вхідні» і в `--dry-run`.

Агент працює не у вашій робочій копії, а в окремому worktree `<worktreesDir>/<slug>`
(за замовчуванням `<repoPath>/../.worktrees/<repo>/<slug>`) на гілці `agent/<slug>`
від свіжого `origin/<baseBranch>`.

### Хто виконує задачу (executors)

У `executors` описуються способи запуску агента. Кожен репозиторій вибирає один через `"executor": "<назва>"`,
інакше береться `defaultExecutor`.

| `type` | Як працює | Після завершення |
|---|---|---|
| `claude-headless` | Mako створює worktree і запускає `claude -p`, чекає результату | Mako комітить, пушить, відкриває PR/MR, пише підсумок у задачу |
| `orca` | `orca worktree create --agent claude --prompt …`: Orca створює workspace, ви бачите агента і керуєте ним | Задача позначається як **«Передано»**; PR робите в Orca |
| `command`, `mode: "managed"` | Будь-яка CLI (Codex, Aider, скрипт) у worktree, Mako чекає завершення | Як у `claude-headless`: код 0 означає успіх, кінець stdout стає підсумком |
| `command`, `mode: "handoff"` | Запускає інструмент і відпускає: вікно tmux, IDE, інший UI агента | **«Передано»** |

```json
"executors": {
  "claude": { "type": "claude-headless", "args": ["--permission-mode", "acceptEdits"] },
  "orca":   { "type": "orca", "agent": "claude" },
  "codex":  { "type": "command", "label": "Codex", "command": "codex", "args": ["exec", "--full-auto", "{prompt}"] },
  "tmux":   { "type": "command", "mode": "handoff", "command": "tmux",
              "args": ["new-window", "-n", "{slug}", "-c", "{worktree}", "claude", "{prompt}"] }
},
"defaultExecutor": "claude"
```

Підстановки для `command`: `{worktree}`, `{prompt}`, `{promptFile}` (промпт у файлі), `{taskId}`, `{slug}`,
`{branch}`, `{title}`, `{url}`, `{repoPath}`, `{repo}`. Команда запускається без shell. Якщо shell потрібен
(`sh -c "…"`), читайте промпт з `{promptFile}`, а не вставляйте `{prompt}` у рядок, бо так безпечніше.

**Orca**: репозиторій має бути доданий в Orca, а CLI `orca` має бути в `PATH`. Mako передає гілку `agent/<slug>`,
базову гілку і посилання на задачу (`--reference`). Якщо версія Orca не приймає посилання такого трекера,
workspace створюється без нього.

Для інтерактивних виконавців (`orca`, `handoff`) промпт трохи інший: агент знає, що ви поруч,
і може спитати, а пуш і PR лишаються на вас.

### Сповіщення

```json
"notifications": { "enabled": true, "autoTake": false, "snoozeMin": 60, "timeoutSec": 600, "inboxPort": 7777 }
```

- `autoTake: true`: брати все без питань.
- `enabled: false`: без сповіщень, лише сторінка «Вхідні».
- **macOS**: кнопки показуються, лише якщо для `terminal-notifier` вибрано стиль **«Оповіщення» (Alerts)**
  у System Settings → Notifications. Інакше сповіщення зникає без кнопок, і відповісти можна у «Вхідних».
- **Windows**: toast-сповіщення з кнопками (SnoreToast).
- **Linux**: `notify-send` з libnotify 0.7.9+ (Ubuntu 20.10+, Fedora 33+).

### Дозволи агента

У `claude-headless` Claude не може спитати дозволу, тому все, що не дозволено в `--allowedTools` (у `executors.<назва>.args`),
просто відхиляється. Розширюйте список під свій стек.

## Запуск

Після `npm run build` доступна команда `mako` (наприклад, через `npm link`).

```bash
npm run dev -- --dry-run   # показати знайдені задачі й промпти, нічого не змінюючи
npm run dev                # працювати постійно (Ctrl+C зупиняє агентів, задачі повернуться в чергу)

npm run build && npm start
```

Worktree після завершення залишаються, щоб їх можна було переглянути. Прибирати їх: `git worktree remove <path>`.
