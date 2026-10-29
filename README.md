# jira-claude-agent

Локальний демон, який моніторить Jira і делегує задачі **Claude Code**, встановленому на вашому компʼютері.
Кожна задача отримує окремий git worktree, приблизно як в Orca.

## Як це працює

```
 Jira ──(JQL poll кожні N сек)──► orchestrator ──► git worktree agent/PROJ-123
                                       │                     │
                                       │            claude -p "<задача>" (headless)
                                       │                     │
   коментар + статус "In Review" ◄─────┴── push гілки + gh pr create
```

1. Раз на `pollIntervalSec` шукає задачі за JQL (за замовчуванням: мітка `claude-agent` і статус «To Do»).
2. Для нової задачі переводить її в `In Progress` і пише коментар.
3. Створює worktree `<worktreesDir>/PROJ-123` на гілці `agent/PROJ-123` від свіжого `origin/<baseBranch>`.
4. Запускає `claude -p` у цьому worktree з промптом, зібраним із summary, description і коментарів задачі.
5. Після завершення: комітить незакомічене, пушить гілку, відкриває PR через `gh` (якщо ввімкнено),
   пише в Jira підсумок агента з посиланням на PR і переводить задачу в `In Review`.
6. Паралельно працює до `maxConcurrent` агентів. Стан зберігається в `.agent-state/state.json`,
   тож після перезапуску задачі не виконуються повторно. Повні логи агента лежать у `.agent-state/logs/<KEY>.jsonl`.

## Налаштування

Потрібні Node.js 20.12+, `claude` (з виконаним входом), `git`, і `gh` (якщо потрібні PR, з виконаним `gh auth login`).

```bash
npm install
cp .env.example .env                              # JIRA_BASE_URL, JIRA_EMAIL, JIRA_API_TOKEN
cp agent.config.example.json agent.config.json    # JQL, статуси, проєкти → репозиторії
```

Що задати в `agent.config.json`:

| Поле | Що означає |
|---|---|
| `jql` | Які задачі віддавати агенту. Наприклад, `assignee = currentUser() AND labels = claude-agent AND statusCategory = "To Do"` |
| `transitions` | Назви статусів (або переходів) для старту, успіху й помилки. `null` означає не змінювати статус |
| `projects.<KEY>` | Ключ Jira-проєкту → локальний репозиторій, базова гілка, тека для worktree, чи створювати PR, додаткові інструкції |
| `claude.args` | Аргументи для `claude`: режим дозволів, `--allowedTools`, `--max-turns`, `--model` тощо |
| `maxConcurrent`, `taskTimeoutMin` | Скільки агентів одночасно і ліміт часу на одну задачу |

### Дозволи агента

У headless-режимі Claude не може спитати дозволу, тому все, що не дозволено в `--allowedTools`,
просто відхиляється. Приклад у конфігу дозволяє редагувати файли й запускати `git`, `npm`, `node`.
Розширюйте список під свій стек. `--dangerously-skip-permissions` використовуйте лише в ізольованому середовищі.

## Запуск

```bash
npm run dev -- --dry-run --once   # показати, які задачі знайдено і який промпт піде агенту, нічого не змінюючи
npm run dev -- --once             # обробити одну порцію задач і завершитися
npm run dev                       # працювати постійно (Ctrl+C зупиняє агентів)

npm run build && npm start        # продакшн-збірка
```

Щоб перезапустити задачу, видаліть її запис із `.agent-state/state.json`.
Worktree залишаються після завершення, щоб їх можна було переглянути. Прибирати їх: `git worktree remove <path>`.
