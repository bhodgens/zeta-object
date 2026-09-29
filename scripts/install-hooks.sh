#!/bin/bash
# Install git hooks for zeta-object.
#
# Configures git to use the project's .githooks directory, makes hooks
# executable, and reports which optional tools are missing (warn, not fail).
# Run from anywhere inside the repo: ./scripts/install-hooks.sh

set -e

PROJECT_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
GITHOOKS_DIR="$PROJECT_ROOT/.githooks"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  zeta-object Git Hooks Installation"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Check if .githooks directory exists
if [ ! -d "$GITHOOKS_DIR" ]; then
  echo "❌ Error: .githooks directory not found at $GITHOOKS_DIR"
  echo ""
  echo "Make sure you're running this from inside the zeta-object repository."
  exit 1
fi

# Configure git to use .githooks
echo "Configuring git to use .githooks directory..."
git config core.hooksPath .githooks

# Verify hooks are executable
echo "Making hooks executable..."
for hook in "$GITHOOKS_DIR"/*; do
  if [ -f "$hook" ]; then
    chmod +x "$hook"
  fi
done
echo "✅ Git hooks configured (core.hooksPath -> .githooks)"
echo ""
echo "Installed hooks:"
for hook in pre-commit pre-commit-vet pre-commit-errors pre-commit-gosec \
            pre-commit-staticcheck pre-push; do
  [ -f "$GITHOOKS_DIR/$hook" ] && echo "  ✓ $hook"
done
echo ""

# Tool availability check (warn, not fail)
echo "Checking tool availability..."
MISSING=""
check_tool() {
  if command -v "$1" >/dev/null 2>&1 || [ -x "$HOME/go/bin/$1" ]; then
    echo "  ✓ $1"
  else
    echo "  ✗ $1 NOT FOUND — $2"
    MISSING="$MISSING $1"
  fi
}

check_tool go        "required for build/vet/test"
check_tool golangci-lint "install with: brew install golangci-lint"
check_tool staticcheck   "install with: go install honnef.co/go/tools/cmd/staticcheck@latest"
check_tool gosec         "install with: go install github.com/securego/gosec/v2/cmd/gosec@latest"
check_tool gitleaks      "install with: brew install gitleaks"
check_tool govulncheck   "install with: go install golang.org/x/vuln/cmd/govulncheck@latest"
check_tool goimports     "install with: go install golang.org/x/tools/cmd/goimports@latest"
echo ""

if [ -n "$MISSING" ]; then
  echo "⚠️  Missing tools:$MISSING"
  echo "   Hooks gracefully skip what is missing, but the full 'make check'"
  echo "   gate expects all of them. This is a warning, not a failure."
else
  echo "✅ All tools available."
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Usage:"
echo "  Hooks run automatically on 'git commit' / 'git push'"
echo "  Skip with --no-verify if needed (not recommended)"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
