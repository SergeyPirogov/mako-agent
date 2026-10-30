# Mako (Go)

Легка версія Mako: один бінарник ~10 МБ, ~10 МБ пам'яті. Лише диспетчер:

1. Шукає в GitLab, GitHub і Jira Cloud задачі, призначені на вас (з міткою-тригером або всі; з `allProjects` — з усіх проєктів).
2. Показує їх на дошці в браузері (`http://127.0.0.1:7777`) і в системних сповіщеннях.
3. «Віддати в Orca»: клонує репозиторій, якщо його ще немає (`~/mako/projects/<name>`), призначає задачу на вас,
   ставить статус In progress і створює workspace в Orca з агентом, задачею й інструкцією. Гілка `feature/…` для
   задач, `fix/…` для багів і GitLab Issue.
4. Коли з гілки відкрито MR/PR: коментар у задачі з посиланням, статус рев'ю (`transitions.onSuccess`), порожній опис MR
   дописується з diff гілки (`claude -p --model haiku`).
5. Після мерджу: статус Done, і задача закривається. MR закрито без мерджу → «Не виконано».
6. Завершені задачі йдуть в архів через `archiveAfterDays` (2 дні).

Вбудованого термінала, плану, автопілота й шаблонів тут немає: агент працює в Orca.

## Запуск

```sh
cd go
go build -o mako ./cmd/mako
./mako -open
```

Конфіг той самий, що в TypeScript-версії: `mako.config.json` і `.env` у `MAKO_HOME`, `~/.mako` або папці застосунку
(`~/Library/Application Support/Mako`). Стан — той самий `.mako/state.json`. Не запускайте обидві версії одночасно:
вони ділять порт і `state.json`.

Токени в `.env`: `GITLAB_TOKEN` (scope `api`), `GITHUB_TOKEN` (або вхід через `gh auth login`),
`JIRA_EMAIL` + `JIRA_API_TOKEN` (+ `JIRA_BASE_URL`). Для задач з Jira MR шукаються на хості `origin` репозиторію
з `GITLAB_TOKEN` або `GITHUB_TOKEN`.

## Розробка

```sh
go vet ./... && go test ./...
```

Тести ганяють диспетчер на імітації GitLab (httptest), а Orca — на фейковому скрипті.
