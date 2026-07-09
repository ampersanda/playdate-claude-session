# Changelog

## 1.1 (2026-07-09)

- "No Sleep" system-menu checkbox (default on) — disables auto-lock so the
  screen stays on while the app runs
- Interactive `setup.sh`: deploys the fly.io server, performs the Claude OAuth
  login, writes `src/config.h`, and builds the `.pdx`
- Screenshot in README

## 1.0

- Initial release: three usage bars (session / all models / per-model) with
  reset countdowns, polled from the bundled Go server on a 1 or 5 minute
  interval
- Hold A for 3 s to force a refresh; ding on every successful fetch
- `stale` badge when the server serves cached data; countdown bar pauses while
  a fetch is in flight
- Go backend for fly.io with a dedicated Claude OAuth session, 60 s cache, and
  a plain-text response format for the Playdate
