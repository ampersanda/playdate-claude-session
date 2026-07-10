# Changelog

## 1.4 (2026-07-11)

- When a refresh lands with the session limit at 100%, play a two-note
  descending minor cue (F#5 → D#5) instead of the usual ding

## 1.3 (2026-07-10)

- When the session bar hits 100%, the polling interval stops and the next
  refresh is scheduled at the session reset time (+60 s buffer); an info line
  ("limit hit - next refresh at reset (…)") shows while waiting
- Fix: a bar at 0% now keeps its outline (previously the whole bar vanished)
- "Sleep on limit" system-menu checkbox (default on) — while the limit is hit,
  auto-lock is re-enabled even with "No Sleep" checked, so the device can
  sleep in low power until the reset (device only; the simulator would just
  blank its window)

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
