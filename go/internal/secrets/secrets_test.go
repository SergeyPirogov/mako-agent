package secrets

import (
	"math/rand"
	"strings"
	"testing"
)

// The tokens are made up at run time: a literal one in the source would be a secret in the repo for every
// scanner, this one included.
func rnd(n int, alphabet string) string {
	r := rand.New(rand.NewSource(int64(n * len(alphabet))))
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return string(b)
}

const (
	alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	upnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b64   = alnum + "+/"
)

func kinds(fs []Finding) string {
	var k []string
	for _, f := range fs {
		k = append(k, f.Kind)
	}
	return strings.Join(k, ", ")
}

func TestEachKind(t *testing.T) {
	for _, c := range []struct{ name, file, line, kind string }{
		{"github classic", "app/main.go", `token := "ghp_` + rnd(36, alnum) + `"`, "GitHub-токен"},
		{"github fine-grained", "deploy.sh", `export GH=github_pat_` + rnd(82, alnum+"_"), "GitHub-токен"},
		{"gitlab", "ci/run.py", `TOKEN = "glpat-` + rnd(20, alnum+"-_") + `"`, "GitLab-токен"},
		{"aws access key", "infra/main.tf", `access_key = "AKIA` + rnd(16, upnum) + `"`, "AWS access key"},
		{"aws secret key", "aws.cfg", `aws_secret_access_key = ` + rnd(40, b64), "AWS secret key"},
		{"slack token", "bot.js", `const slack = "xoxb-` + rnd(12, "0123456789") + "-" + rnd(24, alnum) + `"`, "Slack-токен"},
		{"slack webhook", "notify.rb", `URL = "https://hooks.slack.com/services/T` + rnd(8, upnum) + "/B" + rnd(8, upnum) + "/" + rnd(24, alnum) + `"`, "Slack webhook"},
		{"anthropic", "agent.ts", `const key = "sk-ant-api03-` + rnd(90, alnum+"-_") + `"`, "Anthropic-ключ"},
		{"openai", "llm.py", `client = OpenAI(api_key="sk-proj-` + rnd(48, alnum) + `")`, "OpenAI-ключ"},
		{"google", "maps.js", `const k = "AIza` + rnd(35, alnum+"-_") + `"`, "Google API key"},
		{"stripe", "pay.go", `stripe.Key = "sk_live_` + rnd(24, alnum) + `"`, "Stripe-ключ"},
		{"npm", ".npmrc", `//registry.npmjs.org/:_authToken=npm_` + rnd(36, alnum), "npm-токен"},
		{"telegram", "bot.go", `bot := "123456789:AA` + rnd(33, alnum+"_-") + `"`, "Telegram-бот"},
		{"private key", "id_rsa", "-----BEGIN " + "OPENSSH PRIVATE KEY-----", "приватний ключ"},
		{"password in code", "db/conn.go", `password := "` + rnd(14, alnum) + `"`, "пароль чи секрет у коді (password)"},
		{"password in yaml", "config/prod.yml", `  db_password: ` + rnd(12, alnum), "пароль чи секрет у коді (db_password)"},
		{"secret in json", "settings.json", `"client_secret": "` + rnd(20, alnum) + `",`, "пароль чи секрет у коді (client_secret)"},
		{"high entropy", "app/keys.go", `var signing = []byte("` + rnd(48, b64) + `")`, "випадковий рядок, схожий на ключ"},
	} {
		got := Line(c.file, c.line)
		if len(got) != 1 || got[0].Kind != c.kind {
			t.Errorf("%s: %q → %s, want %s", c.name, c.line, kinds(got), c.kind)
			continue
		}
		if c.kind != "приватний ключ" && !strings.Contains(got[0].Masked, "…(") {
			t.Errorf("%s: masked %q", c.name, got[0].Masked)
		}
	}
}

// The value never shows in full: a few characters and the length.
func TestMask(t *testing.T) {
	v := "ghp_" + rnd(36, alnum)
	m := Mask(v)
	if !strings.HasPrefix(m, "ghp_"+v[4:6]+"…") || !strings.HasSuffix(m, "(40)") || strings.Contains(m, v[6:20]) {
		t.Errorf("mask %q", m)
	}
	if m := Mask("hunter22"); m != "hu…(8)" {
		t.Errorf("mask %q", m)
	}
}

func TestFalsePositives(t *testing.T) {
	for _, c := range []struct{ name, file, line string }{
		{"env reference", "app/config.go", `token := os.Getenv("GITHUB_TOKEN")`},
		{"process.env", "src/api.ts", `const apiKey = process.env.API_KEY;`},
		{"shell variable", "deploy.sh", `PASSWORD="${DB_PASSWORD}"`},
		{"template", "k8s/secret.yaml", `  password: {{ .Values.password }}`},
		{"placeholder", "config/app.yml", `api_key: "your-api-key-here"`},
		{"changeme", "docker-compose.yml", `POSTGRES_PASSWORD: changeme`},
		{"masked", "settings.py", `SECRET = "********"`},
		{"aws docs example", "infra/main.tf", `access_key = "AKIAIOSFODNN7EXAMPLE"`},
		{"fake github token", "app/x.go", `t := "ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"`},
		{"low entropy token", "app/x.go", `t := "ghp_000000000000000000000000000000000000"`},
		{"variable, not value", "auth/login.go", `password = req.Password`},
		{"comparison", "auth/login.go", `if password == "" {`},
		{"about the secret", "app/config.go", `PasswordMinLength = "12345678"`},
		{"token url", "oauth.go", `tokenURL := "https://github.com/login/oauth/access_token"`},
		{"test fixture", "auth/login_test.go", `password := "Sup3rS3cret!"`},
		{"fixture dir", "testdata/users.json", `"password": "Sup3rS3cret!"`},
		{"readme", "README.md", `Set password=Sup3rS3cret! in the config.`},
		{"readme token example", "README.md", "export GITHUB_TOKEN=ghp_your_token_here"},
		{"git sha", "CHANGELOG", `fixed in 3f786850e387550fdab836ed7e6dc881de23001b`},
		{"uuid", "app/ids.go", `const id = "123e4567-e89b-12d3-a456-426614174000"`},
		{"sri hash", "web/index.html", `<script integrity="sha384-` + rnd(64, b64) + `"></script>`},
		{"lockfile", "package-lock.json", `"integrity": "sha512-` + rnd(86, b64) + `=="`},
		{"go.sum", "go.sum", `golang.org/x/net v0.1.0 h1:` + rnd(43, b64) + `=`},
		{"long identifier", "app/x.go", `func handleIncomingWebhookDeliveryRetryWithBackoff() {}`},
		{"path", "app/x.go", `const p = "/usr/local/share/applications/someThing2/more/paths/here"`},
		{"boolean", "config/app.yml", `password_required: true`},
		{"unquoted in code", "app/x.py", `secret = load_secret_from_vault()`},
		{"alphabet", "app/x.go", `strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./")`},
		{"fetch credentials", "web/sw.js", `fetch(url, { method: "POST", credentials: "same-origin" })`},
		{"env var name", "ui/settings.js", `secretField(isLab ? "GITLAB_TOKEN" : "GITHUB_TOKEN", "Токен")`},
		{"token kind", "auth.go", `tokenType := "bearer_token"`},
	} {
		if got := Line(c.file, c.line); len(got) > 0 {
			t.Errorf("%s: %q flagged as %s", c.name, c.line, kinds(got))
		}
	}
}

func TestEnvFiles(t *testing.T) {
	for file, want := range map[string]bool{
		".env": true, "app/.env.local": true, "prod.env": true, ".env.production": true,
		".env.example": false, ".env.sample": false, "config/.env.template": false, "environment.go": false, ".envrc.md": false,
	} {
		if got := isEnvFile(file); got != want {
			t.Errorf("%s: %v", file, got)
		}
	}
}

func TestScanDiff(t *testing.T) {
	token := "ghp_" + rnd(36, alnum)
	diff := strings.Join([]string{
		"commit 0123456789abcdef0123456789abcdef01234567",
		"diff --git a/app/main.go b/app/main.go",
		"--- a/app/main.go",
		"+++ b/app/main.go",
		"@@ -10,0 +11,2 @@ func main() {",
		"+	fmt.Println(\"hi\")",
		"+	token := \"" + token + "\"",
		"@@ -40 +42 @@",
		"-	old := 1",
		"+	new := 2",
		"diff --git a/.env b/.env",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/.env",
		"@@ -0,0 +1 @@",
		"+DEBUG=1",
		"diff --git a/gone.go b/gone.go",
		"--- a/gone.go",
		"+++ /dev/null",
		"@@ -1 +0,0 @@",
		"-	token := \"" + token + "\"",
		"commit ",
		"diff --git a/app/main.go b/app/main.go",
		"+++ b/app/main.go",
		"@@ -11,0 +12 @@",
		"+	token := \"" + token + "\"", // the same line again, uncommitted now: reported once
	}, "\n")
	got := Scan(diff, nil)
	if len(got) != 2 {
		t.Fatalf("findings %+v", got)
	}
	if f := got[0]; f.File != "app/main.go" || f.Line != 12 || f.Commit != "01234567" || f.Kind != "GitHub-токен" || strings.Contains(f.Masked, token[6:]) {
		t.Errorf("token %+v", f)
	}
	if f := got[1]; f.File != ".env" || f.Kind != ".env-файл" || f.Line != 0 {
		t.Errorf("env %+v", f)
	}
	// «Це не секрет» for the token's line: only the .env file is left.
	if left := Scan(diff, map[string]bool{got[0].Key: true}); len(left) != 1 || left[0].Kind != ".env-файл" {
		t.Errorf("allowed %+v", left)
	}
	// A changed line is a new one.
	if Key("app/main.go", `token := "a"`) == Key("app/main.go", `token := "b"`) || Key("a.go", "x") == Key("b.go", "x") {
		t.Error("keys collide")
	}
}
