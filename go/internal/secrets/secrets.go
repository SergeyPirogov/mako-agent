// Package secrets looks for secrets in what an agent is about to push: tokens of the known services, private
// keys, passwords and secrets written as literal values, .env files, and long random strings. Regular
// expressions and entropy, no dependencies.
package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Finding is one secret-like thing in the diff: where it is and what it looks like, never the whole value.
type Finding struct {
	File   string `json:"file"`
	Line   int    `json:"line,omitempty"`   // in the file as the commit has it; 0 for a whole file (.env)
	Commit string `json:"commit,omitempty"` // short hash when the line is in a commit; "" for uncommitted work
	Kind   string `json:"kind"`             // "GitHub-токен", "приватний ключ", ".env-файл"…
	Masked string `json:"masked,omitempty"` // the start of the value and its length: "ghp_Ab…(40)"
	// Key names the line (the file and the line's text): «Це не секрет» allows this key, and the same line is
	// not flagged again.
	Key string `json:"key"`
}

// rule: a format of a known secret. group is the submatch with the value (0: the whole match).
type rule struct {
	kind  string
	re    *regexp.Regexp
	group int
}

var rules = []rule{
	{"приватний ключ", regexp.MustCompile(`-----BEGIN ((RSA|DSA|EC|OPENSSH|PGP|ENCRYPTED) )?PRIVATE KEY( BLOCK)?-----`), 0},
	{"GitHub-токен", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{50,255})\b`), 0},
	{"GitLab-токен", regexp.MustCompile(`\bgl(pat|dt|rt|ptt|cbt|oas|ft|imt|agent|soat)-[A-Za-z0-9_\-]{20,}`), 0},
	{"AWS access key", regexp.MustCompile(`\b(AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`), 0},
	{"AWS secret key", regexp.MustCompile(`(?i)aws.{0,20}secret.{0,20}?['"]?\s*[:=]\s*['"]?([A-Za-z0-9/+]{40})\b`), 1},
	{"Slack-токен", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`), 0},
	{"Slack webhook", regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Za-z0-9_]+/B[A-Za-z0-9_]+/[A-Za-z0-9_]+`), 0},
	{"Anthropic-ключ", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}`), 0},
	{"OpenAI-ключ", regexp.MustCompile(`\bsk-(proj-|svcacct-|admin-)?[A-Za-z0-9_\-]{20,}T3BlbkFJ[A-Za-z0-9_\-]{20,}|\bsk-(proj-|svcacct-|admin-)[A-Za-z0-9_\-]{40,}`), 0},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), 0},
	{"Stripe-ключ", regexp.MustCompile(`\b[sr]k_live_[0-9A-Za-z]{20,}`), 0},
	{"npm-токен", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`), 0},
	{"Telegram-бот", regexp.MustCompile(`\b[0-9]{8,10}:AA[0-9A-Za-z_\-]{33}\b`), 0},
}

// assignment: password=…, secret: "…", api_key = '…'. The name, the quote and the value.
var assignment = regexp.MustCompile(`(?i)([A-Za-z0-9_.\-]*(?:passw(?:or)?d|pwd|secret|token|api[_\-]?key|access[_\-]?key|private[_\-]?key)[A-Za-z0-9_.\-]*)["']?\s*(?::=|=>|[:=])\s*(["'` + "`" + `]?)([^\s"'` + "`" + `,;)}\]]*)`)

// candidate: a long run of the characters keys are made of, for the entropy check.
var candidate = regexp.MustCompile(`[A-Za-z0-9+/_\-=.]{32,}`)

// Placeholders: what a value that is not a real secret looks like.
var placeholder = regexp.MustCompile(`(?i)example|sample|dummy|fake|placeholder|redacted|changeme|change_me|your[_\-]|<|>|xxxx|\*\*\*|\.\.\.|…|test`)

// reference: a value read from somewhere else, not written in: $VAR, ${…}, {{ … }}, %(…)s, os.Getenv(…).
var reference = regexp.MustCompile(`^(\$|%|\{\{|#\{|@|env\b|process\.env|os\.|System\.|getenv|ENV\[|config\.|settings\.|secrets\.|vault:)`)

var keyword = regexp.MustCompile(`^(true|false|null|none|nil|yes|no)$`)

// naming: a value that names something rather than being a secret: an env var (GITHUB_TOKEN), an enum
// ("same-origin", "bearer_token").
var naming = regexp.MustCompile(`^([A-Z][A-Z0-9]*(_[A-Z0-9]+)+|[a-z]+([\-_][a-z]+)+)$`)

// integrity: a subresource or lockfile hash, sha512-…: random, and meant to be public.
var integrity = regexp.MustCompile(`^sha(1|256|384|512)-`)

// Scan reads a diff (git diff or git log -p, -U0 is enough) and reports what of its added lines looks like a
// secret, each line once. allowed: the keys «Це не секрет» let through.
func Scan(diff string, allowed map[string]bool) []Finding {
	var out []Finding
	seen := map[string]bool{}
	add := func(f Finding) {
		if allowed[f.Key] || seen[f.Key+"\x00"+f.Kind] {
			return
		}
		seen[f.Key+"\x00"+f.Kind] = true
		out = append(out, f)
	}
	var commit, file string
	line := 0
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "commit "):
			commit, file = shortHash(strings.TrimSpace(strings.TrimPrefix(l, "commit "))), ""
		case strings.HasPrefix(l, "diff --git "):
			file = ""
		case strings.HasPrefix(l, "+++ "):
			file = ""
			if p := strings.TrimPrefix(l, "+++ "); p != "/dev/null" {
				file = unquote(strings.TrimPrefix(p, "b/"))
				if isEnvFile(file) {
					add(Finding{File: file, Commit: commit, Kind: ".env-файл", Key: Key(file, "")})
				}
			}
		case strings.HasPrefix(l, "@@ "):
			line = hunkStart(l)
		case strings.HasPrefix(l, "+") && file != "":
			for _, f := range Line(file, l[1:]) {
				f.Line, f.Commit = line, commit
				add(f)
			}
			line++
		case strings.HasPrefix(l, " ") && file != "":
			line++
		}
	}
	return out
}

// Line: what of one line of the file looks like a secret; Line and Commit are left for the caller.
func Line(file, text string) []Finding {
	if len(text) > 4000 || skipFile(file) {
		return nil // minified code, a data URI: not where someone writes a token
	}
	key := Key(file, text)
	var out []Finding
	taken := map[string]bool{}
	for _, r := range rules {
		for _, m := range r.re.FindAllStringSubmatch(text, -1) {
			v := m[r.group]
			if r.kind != "приватний ключ" && (placeholder.MatchString(v) || entropy(v) < 3) {
				continue
			}
			taken[v] = true
			f := Finding{File: file, Kind: r.kind, Key: key}
			if r.kind != "приватний ключ" {
				f.Masked = Mask(v)
			}
			out = append(out, f)
		}
	}
	if len(out) > 0 {
		return out
	}
	// The generic checks: not in tests, fixtures and docs, where examples live.
	if exampleFile(file) {
		return nil
	}
	for _, m := range assignment.FindAllStringSubmatch(text, -1) {
		name, quote, v := m[1], m[2], m[3]
		if !literal(file, name, quote, v) {
			continue
		}
		taken[v] = true
		out = append(out, Finding{File: file, Kind: "пароль чи секрет у коді (" + name + ")", Masked: Mask(v), Key: key})
	}
	if len(out) > 0 {
		return out
	}
	for _, v := range candidate.FindAllString(text, -1) {
		v = strings.Trim(v, "=.-_")
		if len(v) < 32 || len(v) > 200 || taken[v] || integrity.MatchString(v) || !random(v) {
			continue
		}
		out = append(out, Finding{File: file, Kind: "випадковий рядок, схожий на ключ", Masked: Mask(v), Key: key})
		break
	}
	return out
}

// literal: the value of password=… is written in, not taken from elsewhere, and is not a placeholder.
func literal(file, name, quote, v string) bool {
	if len(v) < 6 || reference.MatchString(v) || placeholder.MatchString(v) || naming.MatchString(v) || entropy(v) < 2 {
		return false
	}
	lower := strings.ToLower(name)
	// password_hash, token_url, secret_name: about the secret, not the secret.
	for _, w := range []string{"hash", "url", "uri", "file", "path", "name", "type", "len", "length", "field", "header", "env", "id", "count", "min", "max", "prompt", "label", "policy", "endpoint", "expir", "ttl", "regex", "pattern", "required", "enabled", "reset", "confirm", "input"} {
		if strings.HasSuffix(lower, w) || strings.Contains(lower, w+"_") || strings.Contains(lower, w+"-") {
			return false
		}
	}
	if quote != "" {
		return true
	}
	// Unquoted: in a config file a value, in code most often a variable or a call.
	return configFile(file) && !keyword.MatchString(strings.ToLower(v))
}

// random: a string of mixed letters and digits with the entropy of a key, not a word, a path or a hex hash.
func random(v string) bool {
	if strings.Count(v, "/") > 2 || strings.Count(v, ".") > 1 || placeholder.MatchString(v) {
		return false
	}
	var upper, lowerC, digit bool
	for _, c := range v {
		switch {
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lowerC = true
		case c >= '0' && c <= '9':
			digit = true
		}
	}
	return upper && lowerC && digit && entropy(v) >= 4.5 && !sequential(v)
}

// sequential: an alphabet written out, "abcdef…XYZ0123…", is not a key.
func sequential(v string) bool {
	n := 0
	for i := 1; i < len(v); i++ {
		if v[i] == v[i-1]+1 {
			n++
		}
	}
	return n > len(v)/3
}

// entropy: Shannon entropy of the string, bits per character. Hex tops at 4, base64 nears 6.
func entropy(s string) float64 {
	if s == "" {
		return 0
	}
	n := map[rune]float64{}
	total := 0.0
	for _, c := range s {
		n[c]++
		total++
	}
	e := 0.0
	for _, k := range n {
		p := k / total
		e -= p * math.Log2(p)
	}
	return e
}

// Mask: the first few characters and the length, so you can tell which value it is but not use it.
func Mask(v string) string {
	r := []rune(v)
	show := min(4, len(r)/4)
	if p := strings.IndexAny(v, "_-"); p > 0 && p < 12 && p+3 < len(r) {
		show = p + 3 // the prefix that names the kind and two characters more: ghp_Ab…
	}
	return string(r[:show]) + "…(" + strconv.Itoa(len(r)) + ")"
}

// Key: the name of a line, the file and the text; a changed line is a new one.
func Key(file, text string) string {
	h := sha256.Sum256([]byte(file + "\x00" + strings.TrimSpace(text)))
	return hex.EncodeToString(h[:8])
}

// isEnvFile: .env, .env.local, prod.env; not .env.example and the like, which are meant to be committed.
func isEnvFile(file string) bool {
	base := strings.ToLower(path.Base(file))
	if base != ".env" && !strings.HasPrefix(base, ".env.") && !strings.HasSuffix(base, ".env") {
		return false
	}
	for _, w := range []string{"example", "sample", "template", "dist", "defaults", "schema", "test"} {
		if strings.Contains(base, w) {
			return false
		}
	}
	return true
}

// skipFile: files where long random strings are not secrets: lockfiles, sums, maps, images.
func skipFile(file string) bool {
	base := strings.ToLower(path.Base(file))
	switch base {
	case "go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "cargo.lock", "poetry.lock", "gemfile.lock", "composer.lock", "bun.lock", "uv.lock", "flake.lock":
		return true
	}
	for _, ext := range []string{".lock", ".sum", ".map", ".svg", ".min.js", ".min.css", ".snap", ".ipynb"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return false
}

// exampleFile: tests, fixtures and docs, where passwords and keys are examples.
func exampleFile(file string) bool {
	f := "/" + strings.ToLower(file)
	base := path.Base(f)
	for _, d := range []string{"/test/", "/tests/", "/testdata/", "/fixtures/", "/fixture/", "/__tests__/", "/spec/", "/mocks/", "/docs/", "/examples/", "/example/"} {
		if strings.Contains(f, d) {
			return true
		}
	}
	for _, s := range []string{"_test.go", "_test.py", ".md", ".rst", ".adoc", ".txt"} {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return strings.HasPrefix(base, "test_") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

// configFile: where an unquoted value after = or : is a value, not a variable.
func configFile(file string) bool {
	base := strings.ToLower(path.Base(file))
	if isEnvFile(file) || strings.HasPrefix(base, ".env") || base == ".npmrc" || base == ".netrc" || base == ".pypirc" {
		return true
	}
	switch path.Ext(base) {
	case ".env", ".yml", ".yaml", ".properties", ".ini", ".toml", ".cfg", ".conf", ".config", ".tfvars":
		return true
	}
	return false
}

func hunkStart(h string) int {
	// @@ -a,b +c,d @@
	for _, f := range strings.Fields(h) {
		if strings.HasPrefix(f, "+") {
			n, _ := strconv.Atoi(strings.SplitN(f[1:], ",", 2)[0])
			return n
		}
	}
	return 0
}

func shortHash(h string) string {
	if f := strings.Fields(h); len(f) > 0 {
		h = f[0]
	}
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// unquote: git quotes paths with unusual characters: "b/dir/na\303\257ve.txt".
func unquote(p string) string {
	if strings.HasPrefix(p, `"`) {
		if s, err := strconv.Unquote(p); err == nil {
			return strings.TrimPrefix(s, "b/")
		}
	}
	return p
}
