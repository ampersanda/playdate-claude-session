#!/bin/bash
set -e

# SDK path: $PLAYDATE_SDK_PATH, or ~/.Playdate/config (written by the SDK installer)
SDK="${PLAYDATE_SDK_PATH:-$(egrep '^\s*SDKRoot' ~/.Playdate/config 2>/dev/null | head -n 1 | cut -c9-)}"
if [ -z "$SDK" ]; then
    echo "error: Playdate SDK not found; set PLAYDATE_SDK_PATH" >&2
    exit 1
fi

export PLAYDATE_SDK_PATH="$SDK"

make clean 2>/dev/null || true
make

open -a "$SDK/bin/Playdate Simulator.app" ClaudeSession.pdx
