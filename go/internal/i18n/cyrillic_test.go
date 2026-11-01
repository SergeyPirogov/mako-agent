package i18n

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// cyrillicFiles may keep Cyrillic in string literals: the dictionary's uk slot
// and the language detector's word lists.
var cyrillicFiles = map[string]bool{
	"i18n/strings.go": true,
	"i18n/i18n.go":    true,
}

// TestNoCyrillicInStrings keeps the backend English-only: no Cyrillic in
// string literals of non-test Go code. Legacy matchers (old persisted text,
// Ukrainian words in user input) opt out with a `// cyrillic-ok` comment on
// the same line.
func TestNoCyrillicInStrings(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel(filepath.Join(root, "internal"), path)
		rel = filepath.ToSlash(rel)
		if cyrillicFiles[rel] {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		ok := map[int]bool{}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if strings.Contains(c.Text, "cyrillic-ok") {
					ok[fset.Position(c.Pos()).Line] = true
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, isLit := n.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				return true
			}
			for _, r := range lit.Value {
				if unicode.Is(unicode.Cyrillic, r) {
					start := fset.Position(lit.Pos()).Line
					end := fset.Position(lit.End()).Line
					for l := start; l <= end; l++ {
						if ok[l] {
							return true
						}
					}
					t.Errorf("%s:%d: Cyrillic in string literal: %.60s", path, start, lit.Value)
					return true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
