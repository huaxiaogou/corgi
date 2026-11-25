#!/usr/bin/env sh
# Cross-compile self-contained Corgi binaries (with embedded Web UI) and SHA256SUMS.txt.
set -eu

VERSION="${VERSION:-dev}"
OUT_DIR="${OUT_DIR:-dist}"

echo "==> Building embedded Web UI..."
(cd web && npm ci --no-audit --no-fund || npm install --no-audit --no-fund)
(cd web && npm run typecheck && npm run build)

mkdir -p "$OUT_DIR"
rm -f "$OUT_DIR/SHA256SUMS.txt"

for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  BIN="$OUT_DIR/corgi-${GOOS}-${GOARCH}"
  echo "==> Compiling $BIN (version=$VERSION)..."
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -ldflags="-X main.version=$VERSION" -o "$BIN" ./cmd/corgi
done

echo "==> Copying install.sh and computing SHA256SUMS.txt..."
cp scripts/install.sh "$OUT_DIR/install.sh"
(cd "$OUT_DIR" && shasum -a 256 corgi-* > SHA256SUMS.txt)
cat "$OUT_DIR/SHA256SUMS.txt"
