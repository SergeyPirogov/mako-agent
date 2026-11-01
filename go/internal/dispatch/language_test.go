package dispatch

import (
	"context"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// A task written in Russian is not handed to an agent: the issue asks for it in English or Ukrainian and the
// task fails until it is rewritten.
func TestRussianTaskRefused(t *testing.T) {
	d, f, jobs := setup(t)
	ctx := context.Background()
	f.issues[5]["title"] = "Кнопка оплаты не работает"
	f.issues[5]["description"] = "Если нажать на кнопку, ничего не происходит. Нужно это исправить."

	d.Poll(ctx)
	d.Take(ctx, "grp/app#5")
	d.Wait()
	r, _ := d.Store.Get("grp/app#5")
	if r.Status != core.Failed || r.Error != i18n.T("task.russian") {
		t.Fatalf("status %s, error %q", r.Status, r.Error)
	}
	if len(*jobs) != 0 {
		t.Fatalf("handed to an agent: %+v", *jobs)
	}
	if len(f.notes) != 1 || f.notes[0] != i18n.T("comment.rewrite") {
		t.Fatalf("notes %v", f.notes)
	}
	if len(f.statusSet) != 0 {
		t.Fatalf("the tracker's status changed: %v", f.statusSet)
	}
}

// A Ukrainian task quoting Russian, or an English one naming a Russian button, still goes to the agent.
func TestRussianWordsDoNotRefuse(t *testing.T) {
	for _, task := range [][2]string{
		{"Помилка «Объект не найден» у логах", "Після оновлення в логах сервісу з'являється «Объект не найден. Проверьте ещё раз». Треба розібратися, звідки вона береться."},
		{"The «Подтвердить» button does nothing", "In the Russian locale the «Купить сыр» and «Подтвердить заказ» buttons do nothing when clicked."},
	} {
		d, f, jobs := setup(t)
		ctx := context.Background()
		f.issues[5]["title"], f.issues[5]["description"] = task[0], task[1]
		d.Poll(ctx)
		d.Take(ctx, "grp/app#5")
		d.Wait()
		if r, _ := d.Store.Get("grp/app#5"); r.Status != core.HandedOff || len(*jobs) != 1 {
			t.Errorf("%q: status %s, error %q", task[0], r.Status, r.Error)
		}
	}
}
