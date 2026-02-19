#!/usr/bin/env bash
set -e

ROOT="$(cd "$(dirname "$0")" && pwd)"
ANALYZER_DIR="$ROOT/analyzer"
EXT_DIR="$ROOT/extension"
OUT_DIR="$EXT_DIR/out"

echo "══════════════════════════════════════"
echo "  Rex Template Validator — Build"
echo "══════════════════════════════════════"

# ─── Build Go Analyzer ───────────────────
echo ""
echo "▶ Building Go analyzer binary..."
mkdir -p "$OUT_DIR"
cd "$ANALYZER_DIR"

if ! command -v go &>/dev/null; then
  echo "  ⚠ 'go' not found on PATH — skipping analyzer build"
  echo "  Install Go from https://go.dev/dl/ then re-run this script"
else
  EXT=""
  if [[ "$OSTYPE" == "msys" || "$OSTYPE" == "win32" ]]; then
    EXT=".exe"
  fi

  go build -o "$OUT_DIR/rex-analyzer$EXT" .
  echo "  ✓ Analyzer built → out/rex-analyzer$EXT"
fi

# ─── Build TypeScript Extension ──────────
echo ""
echo "▶ Compiling TypeScript extension..."
cd "$EXT_DIR"

if ! command -v npm &>/dev/null; then
  echo "  ⚠ npm not found. Install Node.js from https://nodejs.org/"
  exit 1
fi

# Install deps if needed
if [ ! -d node_modules ]; then
  echo "  Installing npm dependencies..."
  npm install
fi

npx tsc -p tsconfig.json
echo "  ✓ TypeScript compiled → out/"

# ─── Done ────────────────────────────────
echo ""
echo "══════════════════════════════════════"
echo "  Build complete!"
echo ""
echo "  To install in VSCode:"
echo "    1. Open the extension/ folder in VSCode"
echo "    2. Press F5 to launch the Extension Development Host"
echo ""
echo "  Or package as .vsix:"
echo "    cd extension && npx vsce package"
echo "══════════════════════════════════════"
