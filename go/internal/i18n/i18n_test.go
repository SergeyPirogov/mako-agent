package i18n

import (
	"regexp"
	"strings"
	"testing"
)

var verb = regexp.MustCompile(`%[-+# 0]*\d*(\.\d+)?[a-zA-Z%]`)
var named = regexp.MustCompile(`\{\w+\}`)

// Every text is in both languages, with the same fmt verbs (Go) and {names} (the pages) in each.
func TestDictionaryComplete(t *testing.T) {
	for k, v := range dict {
		if strings.TrimSpace(v[0]) == "" || strings.TrimSpace(v[1]) == "" {
			t.Errorf("%s: a language is empty: %q", k, v)
		}
		if a, b := verb.FindAllString(v[0], -1), verb.FindAllString(v[1], -1); strings.Join(a, " ") != strings.Join(b, " ") {
			t.Errorf("%s: verbs %v and %v", k, a, b)
		}
		sorted := func(s string) string {
			m := named.FindAllString(s, -1)
			for i := range m {
				for j := i + 1; j < len(m); j++ {
					if m[j] < m[i] {
						m[i], m[j] = m[j], m[i]
					}
				}
			}
			return strings.Join(m, " ")
		}
		if sorted(v[0]) != sorted(v[1]) {
			t.Errorf("%s: names %q and %q", k, sorted(v[0]), sorted(v[1]))
		}
	}
}

func TestT(t *testing.T) {
	defer SetLang(Lang())
	SetLang(UK)
	if got := T("notify.waiting", "a#1"); got != "✋ a#1: агент чекає на вас" {
		t.Fatalf("uk: %q", got)
	}
	SetLang(EN)
	if got := T("notify.waiting", "a#1"); got != "✋ a#1: the agent is waiting for you" {
		t.Fatalf("en: %q", got)
	}
	SetLang("ru")
	if Lang() != EN {
		t.Fatalf("an unknown language: %q", Lang())
	}
	if got := T("no.such.key"); got != "no.such.key" {
		t.Fatalf("missing key: %q", got)
	}
	if Strings(UK)["s.save"] != "Зберегти" || Strings(EN)["s.save"] != "Save" {
		t.Fatal("Strings")
	}
}

func TestPlural(t *testing.T) {
	defer SetLang(Lang())
	SetLang(UK)
	for n, want := range map[int]string{1: "вид", 2: "види", 5: "видів", 11: "видів", 21: "вид", 24: "види"} {
		if got := Plural(n, "вид", "види", "видів"); got != want {
			t.Errorf("uk %d: %q", n, got)
		}
	}
	SetLang(EN)
	if Plural(1, "kind", "kinds", "kinds") != "kind" || Plural(3, "kind", "kinds", "kinds") != "kinds" {
		t.Error("en")
	}
}

func TestIsRussian(t *testing.T) {
	cases := map[string]bool{
		"Кнопка оплаты не работает. Если нажать на кнопку, ничего не происходит.":    true,
		"Добавь настройку, которая скажет, на каком языке должно быть общение":       true,
		"Кнопка оплати не працює. Якщо натиснути на кнопку, нічого не відбувається.": false,
		"Додай налаштування яке скаже якою мовою має бути спілкування":               false,
		"The payment button does not work when clicked": false,
		"The «Купить сыр» button on the Russian page does nothing: «Выбрать товар» neither, nor «Отмена»": false,
		"Помилка в логах: «Объект не найден» — треба розібратися, звідки вона береться і що її викликає":  false,
		"Сделать авторизацию через гугл для админки, добавить кнопку на страницу логина":                  true,
		"Зробити авторизацію через гугл для адмінки, додати кнопку на сторінку логіна":                    false,
		// A Ukrainian task with a long Russian complaint or log pasted in stays Ukrainian.
		"Користувач пише, що після оновлення не працює вхід. Ось його скарга: Здравствуйте, после обновления я не могу войти в личный кабинет, пишет что пароль неверный, хотя он правильный. Это уже третий раз, исправьте пожалуйста.": false,
		"Падає імпорт:\n```\nОшибка: не удалось прочитать файл, проверьте формат и кодировку. Объект не найден в базе.\n```":                                                                                                             false,
		"Russian is not checked in code: `Ошибка: объект не найден, попробуйте ещё раз позже`":                                                                                                                                           false,
		"Ошибка при сохранении:\n```\nError: object not found\n```\nНужно проверить, почему форма не сохраняется":                                                                                                                        true,
		"":   false,
		"ёы": false, // too short to tell
	}
	for text, want := range cases {
		if got := IsRussian(text); got != want {
			t.Errorf("%q: %v", text, got)
		}
	}
}

func TestAgentNote(t *testing.T) {
	if n := AgentNote(UK); !strings.Contains(n, "in Ukrainian") || !strings.Contains(n, "Russian") {
		t.Errorf("uk: %s", n)
	}
	if n := AgentNote(EN); !strings.Contains(n, "in English") {
		t.Errorf("en: %s", n)
	}
}
