# CLAUDE.md — k8s-cluster-health

Guidance for AI-assisted edits to this repository.

## What this project is

A single-binary, terminal-first Go tool that watches a Kubernetes cluster and alerts (stdout + terminal bell + GNOME `notify-send`) when control-plane or workload instability is detected. Originally written to surface LKE managed-control-plane flaps in real time.

It is a **diagnostic tool**, not a controller, not an operator, not a service. No leader election, no persistence, no HA. Polls on a timer; one process, one cluster.

## Project shape

- `main.go` — the entire program (intentionally single-file).
- `main_test.go` — unit tests for pure functions (parsing, formatting, helpers).
- `go.mod` / `go.sum` — minimal direct deps (`k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go`).
- No `internal/`, `pkg/`, `cmd/` — keep it flat. If `main.go` grows past ~500 lines, split by concern into sibling files in the same `package main`, not into subpackages.

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

## Local connectivity detection

When the API probe errors, do a quick TCP dial to the configured `netCheckTargets` (default `1.1.1.1:443`, `8.8.8.8:443`) with a 1-second per-target timeout. First success wins. If all fail, render `OFFLINE` and skip all incident state machine logic for that tick — pending buffer preserved, open incident preserved, recovery streak preserved. When the next tick succeeds, print `ONLINE — local network restored` and resume.

Don't get clever about diagnosing the *cause* of the local outage (DNS vs. TCP vs. TLS vs. proxy). The dialAny check is a binary "can I reach anything on the public internet" probe; that's enough to distinguish "my wifi is down" from "the cluster is down". For users behind a firewall that blocks both targets, expose `-net-check-targets` so they can substitute reachable hosts; for users who explicitly want every API failure treated as a cluster issue, expose `-no-net-check`.

The injectable `netCheckFunc` field on `watcher` is purely for tests. Don't repurpose it as a runtime override; flags are the user-facing surface.

## What's deliberately absent

Don't add unless the user asks for it:

- Slack / Pushover / PagerDuty / webhook integrations (the brief is "alert on screen").
- Persistence, history files, log rotation.
- Configuration files (the flag set is small enough; no need for YAML / TOML).
- Watch-based reconciliation; polling is the model.
- Subpackages or interfaces; the program is small enough that abstraction adds only cost.
- Dashboards, TUIs, ncurses; output is append-only stdout.

## Threshold tuning

Defaults (`slow=1000ms`, `alert=3000ms`) are calibrated to a managed control plane on a moderate-RTT link, where a healthy `/readyz` lands in 50–250 ms and a flap shows up as multi-second responses or outright EOFs. For larger / busier control planes or higher-RTT links, raise both thresholds. Don't lower them globally to the point that healthy P99s flap to `WARN` — better to leave them and add a dedicated histogram-style mode if needed.

## When you change the alert rendering

- Update `TestBuildNotifyBody` / `TestBuildNotifyBodyTruncatesLargeLists` if you change the body format.
- The `…and N more` truncation pattern matters; the GNOME tray clips long bodies and we want the user to see we suppressed entries.
- Critical urgency makes notifications persist in GNOME's tray until dismissed. Don't downgrade `ALERT` to `normal` urgency without saying why.

## When you change the API probe

- `/readyz?verbose=1` body parsing assumes `[+]` / `[-]` line prefixes. If you point at a different endpoint, update `summarizeReadyz` and its tests.
- The probe must measure wall-clock latency around `httpClient.Do` (not just the time-to-first-byte) — slow body reads from a degraded apiserver are part of what we want to catch.
