package core

import (
	"os"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

func TestMain(m *testing.M) {
	i18n.SetLang(i18n.UK) // what the tests expect Mako to say is Ukrainian
	os.Exit(m.Run())
}
