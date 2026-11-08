#!/usr/bin/env bash
# Install the remote-agents omp extension for discovery + a stable server entry.
#
# - Symlinks the extension entry into ~/.omp/agent/extensions/ (global discovery)
# - Symlinks the workspace node_modules so jiti resolves @remote-agents/protocol
#   from the repo even when the extension is loaded via the symlink
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXT_DIR="${HOME}/.omp/agent/extensions"
BIN_DIR="${HOME}/.local/bin"

mkdir -p "$EXT_DIR" "$BIN_DIR"

# 1. Extension (symlink so repo edits are live)
ln -sfn "$REPO/packages/extension/src/index.ts" "$EXT_DIR/remote-agents.ts"

# 2. Server launcher on PATH (stable entry for ensureServer)
cat > "$BIN_DIR/remote-agents-server" <<EOF
#!/usr/bin/env bash
exec bun "$REPO/packages/server/src/main.ts" "\$@"
EOF
chmod +x "$BIN_DIR/remote-agents-server"

echo "installed:"
echo "  extension: $EXT_DIR/remote-agents.ts -> $REPO/packages/extension/src/index.ts"
echo "  server:    $BIN_DIR/remote-agents-server"
echo
echo "extension resolution note: run 'bun install' in $REPO once so"
echo "@remote-agents/protocol resolves via workspace node_modules."
