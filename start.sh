#!/bin/sh
# AI Agent Room launcher for macOS / Linux (experimental). Same steps as start.bat.
#   Usage: ./start.sh [workdir]
#   - Without workdir: start in the previous workdir (this folder on first run)
#   - With workdir: start with that folder as the workdir
#   - Extra options go in the AI_AGENT_ROOM_OPTS environment variable (e.g. AI_AGENT_ROOM_OPTS="-port 8788 -max-hops 20")
set -u

cd "$(dirname "$0")" || exit 1

CODEX_ARGS="-c sandbox_mode=workspace-write"
export CODEX_ARGS

# If Go is available, build every time so the latest source is used (cached builds finish in seconds)
if command -v go >/dev/null 2>&1; then
  if [ -e ai-agent-room ]; then
    # ai-agent-room in this folder should not be replaced while running. ai-agent-room in other folders is ignored (and not stopped)
    if command -v pgrep >/dev/null 2>&1 && pgrep -f "^$(pwd)/ai-agent-room( |\$)" >/dev/null 2>&1; then
      echo "ai-agent-room is running. Exit it first, then run this script again."
      exit 1
    fi
    # Show files changed since the last build (src and this script), so source changes by agents are not applied silently
    changed=$(find src start.sh -type f \( -name '*.go' -o -name '*.html' -o -name '*.js' -o -name '*.css' -o -name go.mod -o -name go.sum -o -name start.sh \) -newer ai-agent-room 2>/dev/null)
    if [ -n "$changed" ]; then
      echo "$changed" | sed 's/^/  /'
      echo "The files above changed after the last build. Check that there are no unexpected changes."
      if [ -t 0 ]; then
        printf "Build and start with these changes? [y/N] "
        read -r answer
        case "$answer" in
          y|Y) ;;
          *) echo "Canceled."; exit 1 ;;
        esac
      fi
    fi
  fi
  echo "Building..."
  if ! go build -C src -o ../ai-agent-room .; then
    echo "Build failed. If ai-agent-room is running, exit it and run this script again."
    exit 1
  fi
fi

if [ ! -x ai-agent-room ]; then
  echo "ai-agent-room not found. Install Go, or place a prebuilt ai-agent-room in this folder."
  exit 1
fi

if [ $# -gt 0 ]; then
  # shellcheck disable=SC2086 # AI_AGENT_ROOM_OPTS is split into options on purpose
  exec ./ai-agent-room -workdir "$1" ${AI_AGENT_ROOM_OPTS:-}
fi
# shellcheck disable=SC2086
exec ./ai-agent-room ${AI_AGENT_ROOM_OPTS:-}
