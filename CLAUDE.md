# CLAUDE.md — k8s-cluster-health

Guidance for AI-assisted edits to this repository.

## What this project is

A single-binary, terminal-first Go tool that watches a Kubernetes cluster and alerts (stdout + terminal bell + GNOME `notify-send` + optional SMTP2GO email) when control-plane or workload instability is detected. Originally written to surface LKE managed-control-plane flaps in real time.

It is a **diagnostic tool**, not a controller, not an operator, not a service. No leader election, no HA. Polls on a timer; one process, one cluster. The single piece of persisted state is the per-context "last incident" record — purely cosmetic, lets the footer survive restarts.

## Project shape

- `main.go` — main loop, watcher, tick logic, footer, incident state machine.
- `state.go` — per-context state file (`<state-dir>/<context>.json`).
- `email.go` — SMTP2GO email integration + plain-text incident body renderer.
- `main_test.go` — unit tests for pure functions (parsing, formatting, helpers).
- `Dockerfile` / `.dockerignore` — multi-stage build to a distroless static image.
- `go.mod` / `go.sum` — minimal direct deps (`k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go`, `golang.org/x/term`).
- No `internal/`, `pkg/`, `cmd/` — keep it flat. New concerns go in sibling files in the same `package main`, not into subpackages.

## Build / test commands

```sh
go build -o k8s-cluster-health      # build
go test ./...                       # run unit tests
go test -v -run . ./...             # verbose
go run . -context my-cluster -interval 5s  # smoke test against a real cluster
```

Tests exercise pure functions only. The Kubernetes API client is not mocked; live-cluster paths are validated by the smoke run.

## Coding conventions

- Standard Go formatting (`gofmt`); tabs, no trailing whitespace.
- Errors flow through return values; do not panic except in `die()` at startup.
- The watcher must never block its tick loop. Anything that could hang (subprocess execution, network calls outside the per-tick context) goes in a goroutine with its own timeout context.
- Each tick has a per-call `context.WithTimeout` for every API operation. Don't reuse the root context for individual probes.
- Output to stdout uses ANSI escape codes only when `useColor` is true (`isTerminal` check at startup). The `paint()` helper handles this — use it rather than concatenating escape codes inline.
- Notification bodies must be ANSI-stripped (`stripANSI`) before being passed to `notify-send`.

## What to keep stable

- **Flag names and defaults.** Users build muscle memory; renaming `-context`, `-interval`, `-slow-ms`, `-alert-ms`, `-no-bell`, `-no-notify` is a breaking change.
- **Output format of the status line.** It's parseable by eye; treat it as a stable surface. New fields go at the end of the bits list.
- **Severity vocabulary.** `OK` / `WARN` / `ALERT`. Don't introduce a fourth level; tighten thresholds instead.
- **The four signals.** `/readyz` probe, pod restart delta, node Ready, warning events. Adding a fifth signal is fine; removing one is a behaviour change worth justifying.

## Incident state machine

Non-OK ticks accumulate in `w.pending`. An incident opens only when `len(pending) >= minConfirmations` AND at least one pending tick is `ALERT`. WARN-only sequences never escalate. An OK tick while no incident is open clears the pending buffer (single blip → no incident). Once open, every subsequent tick (ALERT/WARN/OK) appends to the incident timeline; the incident closes after `recoveryTicks` consecutive OK ticks. The closing time is the *first* OK after the last non-OK tick, not the last — duration measures how long the cluster was unhealthy. A new non-OK during the recovery streak resets the counter. On program shutdown (`run()` defers `forceCloseIncident`), any open incident is written with `Closed reason: shutdown` so signal-killed sessions don't lose data.

Notification + terminal bell fire only at the moment of escalation (incident open) and again at incident close. There is intentionally no per-tick notification while an incident is open — that's spam, especially when running multiple instances side-by-side.

The report file is markdown with three sections — header, summary, timeline — in that order, for paste-ready use in provider support tickets. Don't reorder these sections; ticketing systems and humans both scan the header first. Keep the file extension `.md` (Linode/AWS/GitHub all render markdown in tickets).

## Sticky footer

Implementation uses DECSTBM (`\x1b[1;<R-1>r`) to define a scrolling region covering rows 1..R-1 with the footer pinned at row R. Footer updates use DECSC/DECRC (`\x1b7`/`\x1b8`) save-and-restore cursor escapes — these are more portable than CSI s/u and work in tmux + most VT100-compatible terminals. **Do not switch to the alternate screen buffer (smcup/`\x1b[?1049h`)** — it disables tmux/terminal scrollback for the duration of the program, which breaks the user's ability to scroll up and see prior log lines. The whole point of using DECSTBM is to keep main-buffer scrollback working.

The footer state is mutex-protected (`footer.mu`) because three callers update or read it: `tick()` from the run loop, `closeIncident()` (also from the run loop, but renders may interleave), and `handleResize()` from the run loop after SIGWINCH bridges through `resizeCh`. Don't render the footer from the SIGWINCH goroutine directly — it bridges through `resizeCh` so all rendering happens on the main goroutine.

A 1-second `footerRefresh` ticker re-renders the footer between poll ticks so `uptime` and the active-incident `elapsed` counter visibly advance. This is intentional UX; don't remove it for "efficiency" — terminal cursor save/restore is microseconds.

The footer is silently disabled when: not a TTY (`!useColor`), terminal too small (<6 rows or <30 cols), or `-no-footer` is passed. In any of those cases, log lines print as before with no scrolling-region setup. `tearDownFooter` (deferred from `main`) resets DECSTBM with `\x1b[r` so the user's shell isn't left with a constrained scrolling region after exit.

## Local connectivity detection

When the API probe errors, do a quick TCP dial to the configured `netCheckTargets` (default `1.1.1.1:443`, `8.8.8.8:443`) with a 1-second per-target timeout. First success wins. If all fail, render `OFFLINE` and skip all incident state machine logic for that tick — pending buffer preserved, open incident preserved, recovery streak preserved. When the next tick succeeds, print `ONLINE — local network restored` and resume.

Don't get clever about diagnosing the *cause* of the local outage (DNS vs. TCP vs. TLS vs. proxy). The dialAny check is a binary "can I reach anything on the public internet" probe; that's enough to distinguish "my wifi is down" from "the cluster is down". For users behind a firewall that blocks both targets, expose `-net-check-targets` so they can substitute reachable hosts; for users who explicitly want every API failure treated as a cluster issue, expose `-no-net-check`.

The injectable `netCheckFunc` field on `watcher` is purely for tests. Don't repurpose it as a runtime override; flags are the user-facing surface.

## State file (per-context "last incident")

`<state-dir>/<context>.json` records `last_incident_end`, `last_incident_duration`, `last_incident_reason`, and a cumulative `incident_count`. Loaded at startup, written atomically (`tmp` + `rename`) on incident close. Purely cosmetic — drives the footer's `last=YYYY-MM-DD HH:MMZ (reason, duration)` field across restarts. The on-disk markdown reports in `-log-dir` remain the source of truth for incident detail.

Default `<state-dir>` is `$XDG_STATE_HOME/k8s-cluster-health` (fallback `~/.local/state/k8s-cluster-health`); `-state-dir` overrides. Context names are sanitised to `[A-Za-z0-9._-]+` for filesystem safety (kubeconfig contexts often contain `:` or `/` — LKE/EKS/GKE all do).

If state-dir resolution or load fails, the watcher logs a `warn:` to stderr and continues with empty state — never fatal. Don't add a fallback to a different location; one path, one obvious failure mode.

## SMTP2GO email integration

Configured by `-email-to` (comma-separated), `-email-from`, and `SMTP2GO_API_KEY` env. All three must be set or `newEmailer` returns `nil` and email is silently disabled.

Sends fire on incident **open** and incident **close** (matching `notify-send` cadence — never per-tick). The body is a plain-text dump of header + tick timeline (`buildIncidentEmailBody`); on close it also includes the path to the markdown report. Subject is `[k8s-cluster-health] <context> ALERT|RECOVERED|SHUTDOWN`.

Sends are launched in goroutines tracked via `watcher.bgWG`; `forceCloseIncident` waits on the WaitGroup so the shutdown-time email actually leaves the process before `main` returns. Failures (HTTP non-2xx or transport error) print `ERR email send failed: …` to stderr and don't block subsequent ticks.

The credential is in env, not a flag, so it doesn't appear in `ps` output or shell history.

## Container packaging

`Dockerfile` is multi-stage: `golang:alpine` builder → `gcr.io/distroless/static-debian12`. The binary is fully static (`CGO_ENABLED=0`). Default entrypoint passes `-log-dir /tmp -state-dir /state` so the only volume the user has to think about is the state directory if they want it to persist across restarts.

`inContainer()` detects `/.dockerenv` (Docker) or `/run/.containerenv` (Podman) and auto-disables bell + `notify-send` so the entrypoint is friendly without extra flags. `-no-container-detect` opts out (e.g., for forwarded DBus sessions).

The image runs as root by default. Distroless `:nonroot` would be marginally better, but stock kubeconfig files (`0600`, owned by user) wouldn't be readable by uid 65532 without a chown step on the host, and that friction isn't worth it for a diagnostic tool.

## What's deliberately absent

Don't add unless the user asks for it:

- Slack / Pushover / PagerDuty / webhook integrations beyond the existing SMTP2GO path.
- History files, log rotation, structured JSON output, or persistence beyond the single per-context state file described above.
- Configuration files (the flag set is small enough; no need for YAML / TOML).
- Watch-based reconciliation; polling is the model.
- Subpackages or interfaces; the program is small enough that abstraction adds only cost.
- Dashboards, TUIs, ncurses; output is append-only stdout.

## Threshold tuning

Defaults (`slow=1000ms`, `alert=3000ms`) are calibrated to a managed control plane on a moderate-RTT link, where a healthy `/readyz` lands in 50–250 ms and a flap shows up as multi-second responses or outright EOFs. For larger / busier control planes or higher-RTT links, raise both thresholds. Don't lower them globally to the point that healthy P99s flap to `WARN` — better to leave them and add a dedicated histogram-style mode if needed.

## When you change the alert rendering

- Update `TestBuildNotifyBody` / `TestBuildNotifyBodyTruncatesLargeLists` if you change the desktop-notification body format.
- Update `TestBuildIncidentEmailBodyOpen` / `TestBuildIncidentEmailBodyClose` if you change the email body format.
- The `…and N more` truncation pattern matters; the GNOME tray clips long bodies and we want the user to see we suppressed entries.
- Critical urgency makes notifications persist in GNOME's tray until dismissed. Don't downgrade `ALERT` to `normal` urgency without saying why.
- Keep the email body plain text. SMTP2GO supports HTML, but plain text renders consistently in every client and is what a paste-into-ticket workflow expects.

## When you change the API probe

- `/readyz?verbose=1` body parsing assumes `[+]` / `[-]` line prefixes. If you point at a different endpoint, update `summarizeReadyz` and its tests.
- The probe must measure wall-clock latency around `httpClient.Do` (not just the time-to-first-byte) — slow body reads from a degraded apiserver are part of what we want to catch.
