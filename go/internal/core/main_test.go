package core

import (
	"os"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

func TestMain(m *testing.M) {
	i18n.SetAgentLanguage(i18n.UK) // what the agents write in these tests is Ukrainian
	os.Exit(m.Run())
}
