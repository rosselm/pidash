#!/usr/bin/env bash
# Cross-compile pidash for every Pi that can run it and publish a GitHub release.
#
#   ./release.sh v0.1.0
#
# Pure standard library means CGO_ENABLED=0 cross-compiles cleanly; no toolchain
# beyond Go itself is involved.
set -euo pipefail

cd "$(dirname "$0")"
VERSION="${1:-}"
GO="${GO:-/usr/local/go/bin/go}"
OUT=dist

[ -n "$VERSION" ] || { echo "usage: $0 vX.Y.Z" >&2; exit 1; }
command -v "$GO" >/dev/null || { echo "go not found at $GO" >&2; exit 1; }
command -v gh >/dev/null   || { echo "gh not found — install the GitHub CLI" >&2; exit 1; }

rm -rf "$OUT"; mkdir -p "$OUT"

build() {
  local arch=$1 arm=$2 name=$3 note=$4
  printf '  %-24s %s\n' "$name" "$note"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOARM="$arm" \
    "$GO" build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o "$OUT/$name" .
}

echo "==> building ${VERSION}"
build arm64 ""  pidash-linux-arm64  "Pi 3/4/5 on 64-bit Raspberry Pi OS"
build arm   7   pidash-linux-armv7  "Pi 2/3/4 on 32-bit Raspberry Pi OS"
build arm   6   pidash-linux-armv6  "Pi 1 / Zero / Zero W"
build amd64 ""  pidash-linux-amd64  "x86-64 Linux (no vcgencmd; thermals fall back to sysfs)"

( cd "$OUT" && sha256sum pidash-* > SHA256SUMS )

cat > "$OUT/NOTES.md" <<NOTES
Download the binary for your board, make it executable, and run it. No runtime
dependencies — the frontend is embedded in the binary.

\`\`\`bash
curl -LO https://github.com/rosselm/pidash/releases/download/${VERSION}/pidash-linux-arm64
chmod +x pidash-linux-arm64
./pidash-linux-arm64 -addr :8090
\`\`\`

Then open \`http://<pi-address>:8090/\`.

| asset | for |
|---|---|
| \`pidash-linux-arm64\` | Pi 3/4/5 on 64-bit Raspberry Pi OS |
| \`pidash-linux-armv7\` | Pi 2/3/4 on 32-bit Raspberry Pi OS |
| \`pidash-linux-armv6\` | Pi 1 / Zero / Zero W |
| \`pidash-linux-amd64\` | x86-64 Linux — runs, but without \`vcgencmd\` there are no throttle flags |

To install it as a systemd service instead, clone the repo and run
\`./install.sh\`; that also sets \`User=\` to whoever runs it and warns about the
group memberships the service needs (\`docker\`, \`video\`, \`adm\`).

Run \`pidash -version\` to confirm what you have, and \`pidash -h\` for the flags.

### Checksums

\`\`\`
$(cat "$OUT/SHA256SUMS")
\`\`\`
NOTES

echo "==> publishing ${VERSION}"
gh release create "$VERSION" "$OUT"/pidash-* "$OUT/SHA256SUMS" \
  --title "$VERSION" --notes-file "$OUT/NOTES.md"
