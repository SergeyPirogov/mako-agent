package i18n

// dict: every text by key, {English, Ukrainian}. Go's texts take fmt verbs (%s, %d); the web pages' take {name}.
var dict = map[string][2]string{}

func init() {
	for _, part := range []map[string][2]string{goStrings, webStrings} {
		for k, v := range part {
			dict[k] = v
		}
	}
}

// goStrings: what Mako itself says outside the pages — notifications, the menu bar, the issues' comments, the
// merge requests and what it asks the agents.
var goStrings = map[string][2]string{
	// Notifications.
	"notify.newTask":              {"🦈 New task", "🦈 Нова задача"},
	"notify.waiting":              {"✋ %s: the agent is waiting for you", "✋ %s: агент чекає на вас"},
	"notify.finished":             {"✅ %s: the agent finished", "✅ %s: агент закінчив"},
	"notify.stopped":              {"⏹ %s: the agent stopped", "⏹ %s: агент зупинився"},
	"notify.attention":            {"⚠️ %s: needs attention", "⚠️ %s: потребує уваги"},
	"notify.checksFailed":         {"The checks before the MR failed: %s", "Перевірка перед MR не пройшла: %s"},
	"notify.reviewPassed":         {"🔍 %s: the agent review passed", "🔍 %s: рев'ю агентом пройдено"},
	"notify.reviewNotes":          {"🔍 %s: the reviewer has remarks", "🔍 %s: є зауваження рецензента"},
	"notify.notHandedOrca":        {"❌ %s: not handed to Orca", "❌ %s: не передано в Orca"},
	"notify.notHandedAgent":       {"❌ %s: not handed to the agent", "❌ %s: не передано агенту"},
	"notify.notTaken":             {"❌ %s: not taken", "❌ %s: не взято в роботу"},
	"notify.handoffCancelled":     {"🦈 %s: the handoff was cancelled", "🦈 %s: передачу скасовано"},
	"notify.handoffCancelledText": {"The workspace is removed, there is no MR. You can retry the task from the board.", "Workspace видалено, MR немає. Задачу можна повторити з дошки."},
	"notify.review":               {"✅ %s: the MR is open", "✅ %s: є MR"},
	"notify.merged":               {"🎉 %s: merged", "🎉 %s: змерджено"},
	"notify.goneClosed":           {"🦈 %s: the task was closed in the tracker", "🦈 %s: задачу закрито в трекері"},
	"notify.goneDeleted":          {"🦈 %s: the task was deleted in the tracker", "🦈 %s: задачу видалено в трекері"},
	"notify.goneText":             {"The agent is still working on it in Orca.", "Агент ще працює над нею в Orca."},
	"notify.reminder":             {"⏰ %s: time to get back to the task", "⏰ %s: час повернутися до задачі"},
	"notify.limitTitle":           {"⏳ The queue is paused: Claude limit", "⏳ Черга на паузі: ліміт Claude"},
	"notify.limitText":            {"The Claude limit (%s) is %d%% used: new tasks wait in the queue until %s.", "Ліміт Claude (%s) використано на %d%%: нові задачі чекають у черзі до %s."},
	"notify.limitWorking":         {" The agents at work (%d) go on, but may stop at the limit.", " Агенти в роботі (%d) продовжують, але можуть зупинитися на ліміті."},
	"notify.limitResetTitle":      {"▶️ The Claude limit has reset", "▶️ Ліміт Claude оновився"},
	"notify.limitResetText":       {"The queue goes on.", "Черга продовжується."},
	"notify.digest":               {"☀️ Overnight summary", "☀️ Підсумок за ніч"},
	"notify.secrets":              {"🔐 %s: push stopped, secrets in the changes", "🔐 %s: пуш зупинено, у змінах секрети"},
	"notify.andMore":              {"- and %d more", "- і ще %d"},
	"notify.commit":               {"commit", "коміт"},
	"notify.reviewAsked":          {"👀 You were asked to review: %s", "👀 Вас призначили рев'ювером: %s"},
	"notify.phoneWorks":           {"Notifications on the phone work", "Сповіщення на телефон працюють"},
	"action.allow":                {"Allow", "Дозволити"},
	"action.deny":                 {"No", "Ні"},
	"action.approvePlan":          {"Approve and do it", "Затвердити й робити"},

	// The overnight summary's line.
	"digest.waiting": {"waiting for you", "чекає на вас"},
	"digest.review":  {"MRs to merge", "MR до мерджу"},
	"digest.failed":  {"failed", "не вдалося"},
	"digest.merged":  {"merged", "змерджено"},

	// The menu bar.
	"tray.openBoard":   {"Open the board", "Відкрити дошку"},
	"tray.openBrowser": {"Open in the browser", "Відкрити в браузері"},
	"tray.settings":    {"Settings…", "Налаштування…"},
	"tray.poll":        {"Check the trackers now", "Перевірити трекери зараз"},
	"tray.pause":       {"Pause: don't look for new tasks", "Пауза: не шукати нових задач"},
	"tray.login":       {"Start at login", "Запускати при вході в систему"},
	"tray.folder":      {"Show the data folder", "Показати папку даних"},
	"tray.restart":     {"Restart when the agents end their turn", "Перезапустити, коли агенти закінчать хід"},
	"tray.quit":        {"Quit Mako", "Вийти з Mako"},
	"tray.newTasks":    {"New tasks: %d", "Нові задачі: %d"},
	"tray.headline":    {"Mako · %d at work", "Mako · в роботі %d"},
	"tray.headlineOf":  {"Mako · %d of %d at work", "Mako · в роботі %d з %d"},
	"tray.paused":      {"⏸ Looking for tasks is paused", "⏸ Пошук задач на паузі"},
	"tray.searching":   {"Looking for tasks…", "Шукаю задачі…"},
	"tray.checkedAt":   {"Checked at %s", "Перевірено о %s"},
	"tray.col.new":     {"new", "нова"},
	"tray.col.doing":   {"at work", "в роботі"},
	"tray.col.review":  {"in review", "на рев'ю"},
	"tray.starting":    {"starting…", "запускаю…"},
	"tray.queued":      {"queued", "у черзі"},
	"tray.waiting":     {"waiting for you", "чекає на вас"},
	"tray.finished":    {"the agent finished", "агент закінчив"},
	"tray.stopped":     {"the agent stopped", "агент зупинився"},

	// Mako's window on macOS.
	"menu.hide":      {"Hide Mako", "Сховати Mako"},
	"menu.edit":      {"Edit", "Редагування"},
	"menu.undo":      {"Undo", "Скасувати"},
	"menu.redo":      {"Redo", "Повторити"},
	"menu.cut":       {"Cut", "Вирізати"},
	"menu.copy":      {"Copy", "Копіювати"},
	"menu.paste":     {"Paste", "Вставити"},
	"menu.selectAll": {"Select All", "Виділити все"},
	"menu.view":      {"View", "Вигляд"},
	"menu.reload":    {"Reload", "Оновити"},
	"menu.window":    {"Window", "Вікно"},
	"menu.close":     {"Close", "Закрити"},
	"menu.minimize":  {"Minimize", "Згорнути"},
	"menu.cancel":    {"Cancel", "Скасувати"},

	// The issue, the merge request, the commits.
	"comment.summary": {"What was done:", "Що зроблено:"},
	"comment.answers": {"Answers to the task's questions:", "Відповіді на питання до задачі:"},
	"comment.rewrite": {"This task is written in Russian. Mako does not take tasks in Russian: please rewrite it in English or Ukrainian, then take it again from Mako's board.",
		"Задачу написано російською. Mako не бере задачі російською: перепишіть її, будь ласка, англійською або українською, а тоді візьміть її знову з дошки Mako."},
	"task.russian":       {"The task is written in Russian: asked in the issue to rewrite it in English or Ukrainian. Once it is rewritten, take it again", "Задачу написано російською: у задачі попрошено переписати її англійською або українською. Коли перепишуть — візьміть її знову"},
	"summary.howChecked": {"## How it was checked", "## Як перевірено"},
	"summary.lookAt":     {"## Worth a look", "## На що звернути увагу"},
	"problem.language":   {"language %q: Mako speaks «en» or «uk»; English for now", "language %q: Mako знає «en» або «uk»; поки що англійська"},
	"mr.whatChanged":     {"## What changed", "## Що змінено"},
	"mr.howChecked":      {"## How it was checked\nMako ran the project's checks in the agent's worktree before opening this merge request:", "## Як перевірено\nMako прогнав перевірки проєкту у worktree агента, перш ніж відкрити цей merge request:"},
	"commit.lesson":      {"%s: Add a review lesson to CLAUDE.md\n\n- %s", "%s: Додати урок з рев'ю в CLAUDE.md\n\n- %s"},

	// What the settings page and the board show as problems.
	"problem.notSetUp":   {"Mako is not set up yet: add a tracker in the Settings", "Mako ще не налаштований: додайте трекер у Налаштуваннях"},
	"problem.noTrackers": {"There are no trackers: add GitLab, GitHub or Jira in the Settings", "Немає жодного трекера: додайте GitLab, GitHub або Jira в Налаштуваннях"},

	// What Mako asks a working agent.
	"agent.push":             {"Commit what you've done (a proper message, as in the instructions) and push the branch %s so the MR has it.", "Закоміть зроблене (нормальне повідомлення, як в інструкції) і запуш гілку %s, щоб MR її мав."},
	"agent.open":             {"Commit what you've done, push the branch %s and open an MR into %s, as the task's instructions say.", "Закоміть зроблене, запуш гілку %s і відкрий MR у %s, як описано в інструкції до задачі."},
	"agent.rebase":           {"Update the branch %s from %s: git fetch origin %s, rebase onto origin/%s, resolve the conflicts if any, run the tests. If the branch is already pushed, push with --force-with-lease.", "Онови гілку %s з %s: git fetch origin %s, rebase на origin/%s, розвʼяжи конфлікти, якщо будуть, прожени тести. Якщо гілка вже запушена, запуш з --force-with-lease."},
	"agent.conflicts":        {"The MR %s conflicts with %s. Rebase the branch %s onto origin/%s, resolve the conflicts, run the tests and push with --force-with-lease.", "MR %s конфліктує з %s. Зроби rebase гілки %s на origin/%s, розвʼяжи конфлікти, прожени тести й запуш з --force-with-lease."},
	"agent.ci":               {"CI on the MR %s failed. Find the cause, fix it, run what failed locally, commit and push.", "CI на MR %s впав. Знайди причину, полагодь, прожени локально те, що впало, закоміть і запуш."},
	"agent.ciJob":            {"Job %s: %s", "Джоба %s: %s"},
	"agent.ciLog":            {"The end of the log:", "Кінець логу:"},
	"agent.comments":         {"The MR %s has unresolved review comments. Fix each one (or explain why not), commit and push the branch %s.", "На MR %s є нерозвʼязані коментарі рев'ю. Виправ кожен (або поясни, чому ні), закоміть і запуш гілку %s."},
	"agent.pushForce":        {"push with --force-with-lease", "запуш з --force-with-lease"},
	"agent.pushPlain":        {"push", "запуш"},
	"agent.howRebase":        {"Rebase the branch %s onto origin/%s", "Зроби rebase гілки %s на origin/%s"},
	"agent.howMerge":         {"Merge origin/%s into the branch %s", "Змерджи origin/%s у гілку %s"},
	"agent.conflictBehind":   {"The branch %s of the MR %s is behind %s and conflicts with it in the files: %s. git fetch origin %s. %s, resolve the conflicts, run the tests and the project's checks, and %s.", "Гілка %s MR %s відстала від %s і конфліктує з нею у файлах: %s. git fetch origin %s. %s, розвʼяжи конфлікти, прожени тести й перевірки проєкту і %s."},
	"agent.reviewCommit":     {"commit", "закоміть"},
	"agent.reviewCommitPush": {"commit and push the branch %s", "закоміть і запуш гілку %s"},
	"agent.reviewNotes":      {"A reviewer (a separate agent, with a clean context) read the task and the diff of the branch %s and has remarks:", "Рецензент (окремий агент, з чистим контекстом) переглянув задачу й diff гілки %s і має зауваження:"},
	"agent.reviewFix":        {"Fix what really is a problem; where you disagree, briefly explain why. Run the tests, %s.", "Виправ те, що справді є проблемою; з чим не згоден — коротко поясни чому. Прожени тести, %s."},
	"agent.checksFailed":     {"Mako ran the checks before the MR (attempt %d of %d), and they failed: %s. The MR is not opened.", "Mako прогнав перевірку перед MR (спроба %d з %d), і вона не пройшла: %s. MR не відкрито."},
	"agent.checkFell":        {"failed", "впала"},
	"agent.checkTimedOut":    {"ran out of time (%d s)", "не вклалась у час (%d с)"},
	"agent.checkTail":        {"`%s` %s, the last lines:", "`%s` %s, останні рядки:"},
	"agent.checksFix":        {"Fix the cause (not the check itself), run it yourself and commit. Don't push and don't open an MR: when you end your turn, Mako checks again and opens the MR.", "Виправ причину (не саму перевірку), прожени її сам і закоміть. Не пушить і не відкривай MR: коли закінчиш хід, Mako перевірить ще раз і відкриє MR."},
}
