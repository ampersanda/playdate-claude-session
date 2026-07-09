# Agent notes

Everything lives in [README.md](README.md) — build/run, layout, behavior,
gotchas (device crash quirks, draw-mode, threading), server API, deploy, and
operations. Read it first; keep it updated when behavior changes.

Quick facts:

- Playdate C app at root (`src/main.c`, single file); Go backend in `server/`
  (deployed on fly.io, see README "Operations")
- Build: `./build.sh`; sideload artifact: `ClaudeSession.pdx.zip` (untracked)
- Secrets: `src/config.h` (gitignored, template `src/config.h.example`),
  `server/.auth_token.local` (gitignored)
- Device debugging: instrument with `logToConsole`, deploy via
  `pdutil <serial> datadisk` + rsync, capture logs over USB serial (pyserial,
  DTR required)
