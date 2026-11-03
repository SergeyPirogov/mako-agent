# Розробка Mako

Mako — один бінарник на Go. Код у `go/`, версія Go — у `go/go.mod`.

## Запуск з коду

```sh
cd go
go build -o mako ./cmd/mako
./mako -open        # -no-tray: без значка в меню
```

`-home <папка>` задає папку з налаштуваннями (за замовчуванням `MAKO_HOME`, `~/.mako` або
`~/Library/Application Support/Mako`). `-readonly` показує дошку без опитування трекерів і агентів — для скриншотів.

## Перевірки

```sh
cd go && gofmt -l . && go vet ./... && go test ./...
```

Тести ганяють диспетчер на імітації GitLab (httptest), а Orca — на фейковому скрипті. CI (`.github/workflows/test.yml`)
робить те саме на Ubuntu і на macOS.

## Файли Mako

Усе лежить у `MAKO_HOME`, `~/.mako` або `~/Library/Application Support/Mako`:

| Файл | Що в ньому |
|---|---|
| `mako.config.json` | налаштування ([приклад](../mako.config.example.json)) |
| `.env` | токени ([приклад](../.env.example)) |
| `.mako/state.json` | стан задач |
| `.mako/mako.log` | журнал |

Токени: `GITLAB_TOKEN` (scope `api`), `GITHUB_TOKEN` (або `gh auth login`),
`JIRA_EMAIL` + `JIRA_API_TOKEN` + `JIRA_BASE_URL`. Для задач з Jira MR шукаються на хості `origin` репозиторію.
Зміни в `/settings` діють без перезапуску.

## Збірка Mako.app

Універсальний бінарник (Apple Silicon + Intel):

```sh
go/packaging/mac/build-app.sh 0.3.0   # → go/dist/Mako.app і Mako-0.3.0-mac.zip
```

Підпис ad-hoc, тож перший запуск — правий клік → «Відкрити».

## Реліз

```sh
git tag v0.3.0 && git push --tags
```

CI (`release.yml`) збере Mako.app і прикріпить `Mako-<версія>-mac.zip` до релізу на GitHub.

## Скриншоти для задач про інтерфейс

`mako screenshot -name before -rev origin/main` і `mako screenshot -name after` знімають дошку з поточної гілки й з main;
з `MAKO_TASK` знімок іде в чат задачі й у трекер. Докладніше: `mako screenshot -h`.
