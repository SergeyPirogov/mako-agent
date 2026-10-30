#!/bin/sh
# Встановлює або оновлює Mako як команду `mako` у терміналі.
#   curl -fsSL https://raw.githubusercontent.com/SergeyPirogov/mako-agent/main/install.sh | sh
# Повторний запуск оновлює Mako до останньої версії.
#   MAKO_DIR   куди покласти код (типово ~/.mako-agent)
#   MAKO_REPO  звідки брати код (типово GitHub)
set -eu

MAKO_DIR="${MAKO_DIR:-$HOME/.mako-agent}"
MAKO_REPO="${MAKO_REPO:-https://github.com/SergeyPirogov/mako-agent}"
BIN_DIR="$HOME/.local/bin"

say() { printf '🦈 %s\n' "$*"; }
fail() { printf '✗ %s\n' "$*" >&2; exit 1; }

command -v git >/dev/null 2>&1 || fail "Потрібен git. На Mac: xcode-select --install, у Linux: встановіть пакет git."

if ! command -v node >/dev/null 2>&1; then
  fail "Потрібен Node.js 22 або новіший. На Mac: brew install node, або через nvm: https://github.com/nvm-sh/nvm"
fi
NODE_MAJOR=$(node -p 'process.versions.node.split(".")[0]')
if [ "$NODE_MAJOR" -lt 22 ]; then
  fail "Потрібен Node.js 22 або новіший, а встановлено $(node -v). Оновіть: brew upgrade node або nvm install 22"
fi
command -v npm >/dev/null 2>&1 || fail "Не знайдено npm (зазвичай іде разом із Node.js)."

if [ -d "$MAKO_DIR/.git" ]; then
  say "Оновлюю Mako у $MAKO_DIR"
  git -C "$MAKO_DIR" pull --ff-only --quiet
else
  [ -e "$MAKO_DIR" ] && fail "$MAKO_DIR вже існує і це не git-репозиторій. Видаліть його або вкажіть інший MAKO_DIR."
  say "Завантажую Mako у $MAKO_DIR"
  git clone --depth 1 --quiet "$MAKO_REPO" "$MAKO_DIR"
fi

say "Встановлюю залежності (це може зайняти хвилину)"
# Electron потрібен лише застосунку для Mac, команді mako він не потрібен.
(cd "$MAKO_DIR" && ELECTRON_SKIP_BINARY_DOWNLOAD=1 npm ci --no-audit --no-fund --loglevel=error >/dev/null)
say "Збираю"
(cd "$MAKO_DIR" && npm run build --silent >/dev/null)

mkdir -p "$BIN_DIR"
cat > "$BIN_DIR/mako" <<EOF
#!/bin/sh
# Створено install.sh. Налаштування і стан Mako лежать у \${MAKO_HOME:-\$HOME/.mako}.
MAKO_HOME="\${MAKO_HOME:-\$HOME/.mako}"
mkdir -p "\$MAKO_HOME"
cd "\$MAKO_HOME" || exit 1
exec node "$MAKO_DIR/dist/index.js" "\$@"
EOF
chmod +x "$BIN_DIR/mako"

VERSION=$(git -C "$MAKO_DIR" rev-parse --short HEAD)
say "Готово: Mako ($VERSION) встановлено як $BIN_DIR/mako"
say "Налаштування зберігатимуться в ~/.mako"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *)
    case "${SHELL:-}" in
      */zsh) RC="$HOME/.zshrc" ;;
      */bash) RC="$HOME/.bashrc" ;;
      *) RC="$HOME/.profile" ;;
    esac
    printf '\n⚠ %s немає в PATH. Додайте його й відкрийте новий термінал:\n' "$BIN_DIR"
    printf "    echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> %s\n" "$RC"
    ;;
esac

if ! command -v claude >/dev/null 2>&1; then
  printf '\n⚠ Не знайдено Claude Code (команда claude). Встановіть його:\n'
  printf '    npm install -g @anthropic-ai/claude-code\n'
fi

printf '\nЗапуск: mako  (відкриє налаштування в браузері)\nОновлення: запустіть цей скрипт ще раз\n'
