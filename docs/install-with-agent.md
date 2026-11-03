# Встановлення Mako через агента

Дайте цей промпт Claude Code, і він поставить Mako з останнього релізу. Токени ви впишете самі в `/settings`:
агент їх не бачить.

````text
Встанови Mako (https://github.com/SergeyPirogov/mako-agent) на цей Mac з останнього релізу. Роби по кроках, відповідай українською.

1. Перевір, що це macOS і що в PATH є `gh` (і він залогінений: `gh auth status`) та `claude`.
   Чогось бракує — скажи, що саме і як поставити, і зупинись. Нічого не став без мого дозволу.
2. Завантаж останній реліз:
   `gh release download --repo SergeyPirogov/mako-agent --pattern '*-mac.zip' --dir ~/Downloads --clobber`.
3. Розпакуй архів (`ditto -x -k ~/Downloads/Mako-*-mac.zip ~/Downloads/Mako`) і перенеси `Mako.app` у `/Applications`
   (якщо стара версія вже є, спершу закрий Mako і заміни її).
4. Підпис ad-hoc: зніми карантин `xattr -dr com.apple.quarantine /Applications/Mako.app` або підкажи мені
   перший запуск через правий клік → «Відкрити».
5. Запусти `open /Applications/Mako.app` і перевір, що `http://127.0.0.1:7777` відповідає (`curl -sI http://127.0.0.1:7777`).
   Не відповідає — подивись журнал `.mako/mako.log` у папці Mako (`~/.mako` або `~/Library/Application Support/Mako`)
   і розбери причину.
6. Наприкінці скажи, що Mako відкрилась. Токени (`GITLAB_TOKEN` зі scope `api`, `GITHUB_TOKEN` або `gh auth login`,
   `JIRA_EMAIL` + `JIRA_API_TOKEN` + `JIRA_BASE_URL`), проєкти, агента, сповіщення й телефон я налаштую в `/settings`.
   Токенів не питай у чаті й не читай `.env`.
````
