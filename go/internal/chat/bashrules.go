package chat

import (
	"encoding/json"
	"strings"
)

// subcommanded: the programs whose second word says what they do (git add, go test, gh pr): a rule for them keeps
// it, so «always» for `git add` does not let `git push` through too.
var subcommanded = map[string]bool{"git": true, "gh": true, "glab": true, "go": true, "npm": true, "pnpm": true,
	"yarn": true, "npx": true, "cargo": true, "docker": true, "kubectl": true, "brew": true}

// BashRules: the Claude Code rules ("git add:*") that allow each command of a shell line, in their order, each
// once. Claude Code checks a compound line (`cd x && git add -A && git commit -F - <<'EOF'`) command by command
// and suggests no rule for it, so Mako makes them: quoted text and a heredoc's body are not commands, and a
// redirection such as 2>&1 does not split one.
func BashRules(line string) []string {
	var rules []string
	seen := map[string]bool{}
	for _, words := range bashCommands(line) {
		for len(words) > 0 && isAssignment(words[0]) {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		rule := words[0] + ":*"
		if subcommanded[words[0]] && len(words) > 1 && !strings.HasPrefix(words[1], "-") {
			rule = words[0] + " " + words[1] + ":*"
		}
		if strings.ContainsAny(words[0], "$`(){}") || seen[rule] {
			continue
		}
		seen[rule] = true
		rules = append(rules, rule)
	}
	return rules
}

// bashCommands splits a shell line into its commands' words.
func bashCommands(line string) [][]string {
	var cmds [][]string
	var words []string
	var word strings.Builder
	inWord := false
	var heredocs []string // the delimiters whose bodies start on the next line
	flushWord := func() {
		if inWord {
			words = append(words, word.String())
		}
		word.Reset()
		inWord = false
	}
	flushCmd := func() {
		flushWord()
		if len(words) > 0 {
			cmds = append(cmds, words)
		}
		words = nil
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'' || c == '"':
			inWord = true
			j := i + 1
			for j < len(rs) && rs[j] != c {
				if c == '"' && rs[j] == '\\' && j+1 < len(rs) {
					j++
				}
				if c == '"' && isHeredoc(rs, j) {
					// "$(cat <<'EOF' … EOF)": the body is text whatever quotes it holds; the string goes on after it.
					d, k := heredocDelim(rs, j)
					j = skipHeredoc(rs, lineEnd(rs, k), d)
					continue
				}
				word.WriteRune(rs[j])
				j++
			}
			i = j
		case c == '\\' && i+1 < len(rs):
			if rs[i+1] != '\n' {
				inWord = true
				word.WriteRune(rs[i+1])
			}
			i++
		case isHeredoc(rs, i):
			// A heredoc: its delimiter (quoted or not) is the next word; its body is skipped below.
			flushWord()
			d, j := heredocDelim(rs, i)
			heredocs = append(heredocs, d)
			i = j - 1
		case c == '\n':
			flushCmd()
			for _, d := range heredocs {
				i = skipHeredoc(rs, i, d)
			}
			heredocs = nil
		case c == '&' && (i > 0 && rs[i-1] == '>' || i+1 < len(rs) && rs[i+1] == '>'):
			// 2>&1, &>file: a redirection, not a command in the background.
			inWord = true
			word.WriteRune(c)
		case strings.ContainsRune(";&|()", c):
			flushCmd()
		case c == ' ' || c == '\t':
			flushWord()
		default:
			inWord = true
			word.WriteRune(c)
		}
	}
	flushCmd()
	return cmds
}

// isHeredoc: a heredoc starts at i (<<, not <<<).
func isHeredoc(rs []rune, i int) bool {
	return rs[i] == '<' && i+1 < len(rs) && rs[i+1] == '<' && (i+2 >= len(rs) || rs[i+2] != '<')
}

// heredocDelim reads the delimiter of the heredoc that starts at i, without its quotes, and where it ends.
func heredocDelim(rs []rune, i int) (string, int) {
	j := i + 2
	if j < len(rs) && rs[j] == '-' {
		j++
	}
	for j < len(rs) && rs[j] == ' ' {
		j++
	}
	var d strings.Builder
	for j < len(rs) && !strings.ContainsRune(" \t\n;&|<>()", rs[j]) {
		if rs[j] != '\'' && rs[j] != '"' {
			d.WriteRune(rs[j])
		}
		j++
	}
	return d.String(), j
}

// lineEnd: where the line that i is on ends (its newline, or the end).
func lineEnd(rs []rune, i int) int {
	for i < len(rs) && rs[i] != '\n' {
		i++
	}
	return i
}

// skipHeredoc: from the newline at nl, past the body lines up to the one that is the delimiter d; the newline that
// ends it (or the end of the line).
func skipHeredoc(rs []rune, nl int, d string) int {
	for nl < len(rs) {
		end := lineEnd(rs, nl+1)
		body := strings.TrimSpace(string(rs[min(nl+1, len(rs)):end]))
		nl = end
		if body == d {
			break
		}
	}
	return nl
}

func isAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	return eq > 0 && !strings.ContainsAny(w[:eq], "-/.:")
}

// bashSuggestions: what «Always allow» adds when Claude Code suggests nothing for a Bash line.
func bashSuggestions(input json.RawMessage) json.RawMessage {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	rules := []map[string]string{}
	for _, r := range BashRules(in.Command) {
		rules = append(rules, map[string]string{"toolName": "Bash", "ruleContent": r})
	}
	if len(rules) == 0 {
		return nil
	}
	b, _ := json.Marshal([]map[string]any{{"type": "addRules", "rules": rules, "behavior": "allow", "destination": "session"}})
	return b
}

// SuggestedRules: the allow rules of a request's suggestions as --allowedTools takes them: "Bash(git add:*)", "WebFetch".
func SuggestedRules(suggestions json.RawMessage) []string {
	var sug []struct {
		Type     string `json:"type"`
		Behavior string `json:"behavior"`
		Rules    []struct {
			ToolName    string `json:"toolName"`
			RuleContent string `json:"ruleContent"`
		} `json:"rules"`
	}
	if json.Unmarshal(suggestions, &sug) != nil {
		return nil
	}
	var out []string
	for _, s := range sug {
		if s.Type != "addRules" || s.Behavior != "allow" {
			continue
		}
		for _, r := range s.Rules {
			if r.ToolName == "" {
				continue
			}
			if r.RuleContent != "" {
				out = append(out, r.ToolName+"("+r.RuleContent+")")
			} else {
				out = append(out, r.ToolName)
			}
		}
	}
	return out
}

// AlwaysRules: the rules «Always allow» on this request keeps; none when it is not waiting or suggests none.
func (s *Session) AlwaysRules(requestID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[requestID]; !ok {
		return nil
	}
	for i := range s.items {
		if s.items[i].Kind == "permission" && s.items[i].ID == requestID {
			return SuggestedRules(s.items[i].Suggestions)
		}
	}
	return nil
}
