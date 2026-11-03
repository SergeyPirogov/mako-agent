package web

import (
	"regexp"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// Every key the pages ask t() for, in a call or a data-t attribute, is in the web strings: a missing one shows up
// to the user as the key itself ("tray.poll"). Keys built at runtime end with a dot ("b.col.") and are not checked.
func TestPagesUseKnownKeys(t *testing.T) {
	call := regexp.MustCompile(`\bt\("((?:b|s|theme)\.[A-Za-z0-9_.]+)"`)
	attr := regexp.MustCompile(`data-t(?:-title|-ph)?="([^"]+)"`)
	strs := i18n.WebStrings()
	for name, page := range map[string]string{"board.html": string(board), "settings.html": string(settingsPage)} {
		seen := map[string]bool{}
		for _, re := range []*regexp.Regexp{call, attr} {
			for _, m := range re.FindAllStringSubmatch(page, -1) {
				key := m[1]
				if seen[key] || strings.HasSuffix(key, ".") {
					continue
				}
				seen[key] = true
				if _, ok := strs[key]; !ok {
					t.Errorf("%s asks for %q, which is not in the web strings", name, key)
				}
			}
		}
		if len(seen) < 15 {
			t.Errorf("%s: only %d keys found, the patterns are off", name, len(seen))
		}
	}
}
