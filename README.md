# k8s-cluster-health

Single-binary Kubernetes cluster health watcher. Polls the API server, pods, nodes, and warning events on an interval, prints a colourised status line per tick, and raises terminal + GNOME desktop notifications + optional SMTP2GO email when instability is detected.

Built originally to spot LKE (Linode Kubernetes Engine) managed-control-plane flaps in real time — `etcd-readiness failed`, slow `/readyz` responses, and the cascading restart loops they cause across `calico-kube-controllers`, `csi-*`, `cert-manager`, `kube-state-metrics`, etc.

## Install

```sh
git clone https://github.com/johnvanham/k8s-cluster-health.git
cd k8s-cluster-health
go build -o k8s-cluster-health
```

Requires Go 1.25+. The binary is ~50 MB (statically links `client-go`).

## Usage

```sh
# Watch the current kubeconfig context.
./k8s-cluster-health

# Watch a specific context.
./k8s-cluster-health -context my-cluster

# Tighter detection: poll every 5s, warn over 500ms, alert over 2s.
./k8s-cluster-health -context my-cluster -interval 5s -slow-ms 500 -alert-ms 2000

# All flags.
./k8s-cluster-health -h
```

| Flag | Default | Purpose |
|---|---|---|
| `-context` | current-context from kubeconfig | Kubeconfig context to watch |
| `-kubeconfig` | `$KUBECONFIG` / `~/.kube/config` | Kubeconfig path(s) |
| `-interval` | `10s` | How often to poll |
| `-slow-ms` | `1000` | API latency threshold for `WARN` |
| `-alert-ms` | `3000` | API latency threshold for `ALERT` |
| `-no-bell` | off | Suppress terminal bell on alerts |
| `-no-notify` | off | Suppress GNOME desktop notifications |
| `-log-dir` | `.` | Directory to write incident reports |
| `-recovery-ticks` | `3` | Consecutive OK ticks required to close an incident |
| `-min-confirmations` | `2` | Consecutive non-OK ticks (with at least one ALERT) required to open an incident |
| `-net-check-targets` | `1.1.1.1:443,8.8.8.8:443` | Comma-separated TCP targets used to detect local connectivity loss; first success wins |
| `-no-net-check` | off | Disable local-network detection (every API failure becomes a cluster issue) |
| `-no-footer` | off | Disable the sticky status footer |
| `-state-dir` | `$XDG_STATE_HOME/k8s-cluster-health` (or `~/.local/state/...`) | Directory for per-context state files (`<context>.json`) |
| `-email-to` | unset | Comma-separated recipients for SMTP2GO email alerts (requires `SMTP2GO_API_KEY` env + `-email-from`) |
| `-email-from` | unset | Sender address for SMTP2GO email alerts |
| `-no-container-detect` | off | Skip auto-disabling `-no-bell` / `-no-notify` when running inside a container |

Environment variables:

| Var | Purpose |
|---|---|
| `SMTP2GO_API_KEY` | API key for SMTP2GO. Email alerts are silently disabled when unset. Kept out of flags so it doesn't show up in `ps` or shell history. |
| `XDG_STATE_HOME` | Standard XDG location used as the default state directory parent. |

## How it works

Every `interval`, the watcher runs four checks in sequence and renders one status line:

1. **API server `/readyz?verbose=1` probe** — direct HTTPS call using credentials from the resolved kubeconfig context (via `rest.HTTPClientFor`). Captures both the wall-clock latency and the body, which lists every readiness gate (`[+]gate ok` / `[-]gate failed: …`). This is what surfaces `etcd-readiness failed` directly, and naturally captures `EOF` / `context deadline exceeded` from a flapping managed control plane.

2. **Pod inventory** (`CoreV1().Pods("").List`) — sums `RestartCount` across all containers per pod and compares to the snapshot from the previous tick. The first tick only seeds the snapshot; deltas alert from the second tick onward. Deleted pods are pruned from the snapshot.

3. **Node Ready conditions** (`CoreV1().Nodes().List`) — flags any node whose `Ready` condition is not `True`.

4. **Warning events in the last `2 × interval`** (`CoreV1().Events("").List` with `fieldSelector=type=Warning`) — deduplicated by `UID + lastTimestamp` so a coalesced burst fires once per actual occurrence. Events older than 1 hour are pruned from the dedup map.

The tick severity is the worst signal seen:

- `OK` (green) — everything within thresholds, no deltas, no events.
- `WARN` (yellow) — API latency between `slow-ms` and `alert-ms`, no other signals.
- `ALERT` (red) — API latency above `alert-ms`, or `/readyz` non-2xx, or any pod restart, NotReady node, or new warning event.

On `ALERT` ticks (and `WARN` for slow-API), it pushes a `notify-send` desktop notification (critical / normal urgency) with up to four restart entries and four event entries, plus the API failure summary. The terminal bell is also rung on `ALERT` ticks if the output is a TTY. The notification is sent in a goroutine with a 5s timeout — never blocks the poll loop.

### Incident reports

The watcher debounces non-OK ticks before escalating, so a single transient blip (a slow `/readyz` probe, a one-off pod restart, a brief liveness probe failure) doesn't fire a notification. An incident opens only when the buffered non-OK ticks reach `-min-confirmations` (default 2) **and** at least one of them is an ALERT. WARN-only stretches don't open incidents — sustained slow API is still visible on the tick line, but doesn't escalate without a hard failure.

- **Start time** = timestamp of the *first* tick in the buffer that led to escalation (the WARN preamble is included if it preceded the confirming ALERT — providers see the lead-in).
- **End time** = timestamp of the *first* OK tick after the last non-OK tick (so duration reflects how long the cluster was actually unhealthy, not how long the recovery probe ran).
- **Recovery** closes the incident after `-recovery-ticks` (default 3) consecutive OK ticks.
- **Closed reason** = `recovered` for normal close, `shutdown` if the program is killed mid-incident (a partial report is still written so SIGINT doesn't lose data).
- A new ALERT or WARN during the recovery streak resets the OK counter — the incident stays open.

Examples (`-min-confirmations 2`):

| Tick sequence | Result |
|---|---|
| `ALERT, OK` | Single blip suppressed; nothing recorded. |
| `ALERT, ALERT` | Incident opens at the second ALERT, includes both ticks in the timeline. |
| `WARN, ALERT` | Incident opens; WARN is included as preamble. |
| `WARN, WARN, WARN, …` | Never opens an incident (no ALERT in buffer). |
| `ALERT, OK, ALERT` | Two isolated blips; OK clears the buffer; nothing opens. |

Notification + terminal bell fire **once** at incident open and **once** at incident close — no per-tick spam during a long incident.

Set `-min-confirmations 1` to restore "fire on first ALERT" behaviour.

The report has three sections, ordered for ticket-pasting:

1. **Header** — context, API server, start/end (UTC, RFC 3339), duration, close reason.
2. **Summary** — tick severity counts, max API latency, deduplicated API failure signatures with counts, distinct pods that restarted (with cumulative delta and final restart count), NotReady nodes, distinct Warning event reasons with their first observed message.
3. **Timeline** — full per-tick detail: API status, NotReady nodes, pod restarts, individual warning events.

Sample header (truncated):

```markdown
# Cluster instability incident — 2026-04-25T12:34:56Z

- **Cluster context:** `my-cluster`
- **API server:** `https://kube-api.example.local:6443`
- **Started (UTC):** 2026-04-25T12:34:56Z
- **Ended (UTC):** 2026-04-25T12:42:11Z
- **Duration:** 7m15s
- **Closed reason:** recovered
```

### Sample output

```
k8s-cluster-health context=my-cluster server=https://kube-api.example.local:6443 interval=5s slow=1000ms alert=3000ms confirm=2 recover=3 notify=on net-check=1.1.1.1:443,8.8.8.8:443 email=off state=/home/me/.local/state/k8s-cluster-health/my-cluster.json
────────────────────────────────────────────────────────────────────────────────
[14:01:25Z] my-cluster OK    api=92ms  pods=75/75 nodes=ready new-warn-evt=0
[14:01:30Z] my-cluster ALERT api=8127ms pods=73/75 nodes=ready new-warn-evt=4
       └── readyz FAIL [HTTP 500] etcd-readiness failed: reason withheld
       └── restart kube-system/calico-kube-controllers-689c764695-8wbx8 +1 (now 13)
       └── event kube-system/pod/calico-kube-controllers-... [Unhealthy] Liveness probe failed: …
       └── event kube-system/pod/coredns-... [FailedCreatePodSandBox] plugin type=calico failed (add): …
[14:01:35Z] my-cluster ALERT api=ERR Get "https://kube-api.example.local:6443/readyz?verbose=1": EOF
```

Each tick line includes the context name after the timestamp so you can run multiple instances in tmux panes and see at a glance which cluster is reporting.

### Sticky status footer

When stdout is a TTY, the watcher pins a single-line status footer to the bottom of the terminal. It shows live state at a glance:

```
ctx=my-cluster │ status=OK │ api avg=87ms min=14ms max=521ms │ pods=75/75 │ nodes=ready │ warns=2 │ incidents=0 │ uptime=12m4s
```

During an active incident the footer changes to:

```
ctx=my-cluster │ status=ALERT │ api avg=120ms min=14ms max=8127ms │ pods=73/75 │ nodes=ready │ INCIDENT 2m15s (14 ticks) │ warns=5 │ incidents=2 │ uptime=1h7m
```

After an incident closes:

```
ctx=my-cluster │ status=OK │ api avg=92ms min=14ms max=521ms │ pods=75/75 │ nodes=ready │ last=2026-04-25 14:42Z (recovered, 7m12s) │ warns=8 │ incidents=3 │ uptime=1h14m
```

The `last=…` field shows full date + time so the user can see at a glance whether the most-recent incident was today or weeks ago. It is loaded from the per-context state file at startup, so it survives restarts of the watcher.

The `api` field shows session-wide statistics for successful API probes: arithmetic mean (`avg`), minimum (`min`), and maximum (`max`) latency in milliseconds. Failed probes (HTTP non-2xx, transport errors, EOF, timeout) are deliberately excluded so a flap doesn't pollute the baseline. Before the first successful sample, the field collapses to `api=<latest>ms`. All counters reset each time the program restarts.

The `warns` counter ticks every WARN-severity tick (API latency between `slow-ms` and `alert-ms` with no other anomaly), even ones that never escalated to opening an incident. This surfaces sustained-but-quiet API slowness over a session. ALERT-severity ticks that didn't reach `min-confirmations` are *not* included — the log line shows them, the warn counter does not.

Implementation: the watcher sets a DECSTBM scrolling region covering rows 1..R-1 of the terminal, and re-renders the footer at row R using save/restore cursor escapes. **Log output scrolls within the upper region only — the footer stays pinned, and tmux scrollback still captures everything that scrolls out, so scrolling up to see prior history works exactly as before.** `uptime` and the live `INCIDENT <elapsed>` counter advance every second even between poll ticks.

The footer is automatically disabled when stdout isn't a TTY (e.g., piped to `tee`), when the terminal is too small (<6 rows or <30 cols), or when `-no-footer` is set. SIGWINCH (terminal resize) is handled — the scrolling region is reset to the new dimensions and the footer is redrawn.

### Local connectivity detection

If the API probe errors out (EOF, dial timeout, DNS failure), the watcher does a quick TCP dial to a list of well-known external hosts (`-net-check-targets`, default `1.1.1.1:443,8.8.8.8:443`) with a 1-second timeout per target. First success wins. If *all* targets fail, the local machine is presumed offline; the tick renders as `OFFLINE` and the incident state machine is **paused** for that tick — no escalation, no notifications, no changes to the pending buffer or any open incident.

When the local network comes back, a one-line `ONLINE — local network restored` is printed and normal polling resumes. This means:

- A wifi blip during a tick → `OFFLINE` line, no false alarm.
- A wifi blip during an open incident → incident timeline is preserved; no spurious "restart" or "event" entries get appended; recovery streak isn't reset.
- A long local outage (laptop suspend, train tunnel, etc.) → repeated `OFFLINE` lines but no incident churn; clean resumption when connectivity returns.

Pass `-no-net-check` to disable this entirely (every API failure is then treated as a real cluster issue, like before).

### Persistent state file

The watcher records the most-recent incident per kubeconfig context to `<state-dir>/<context>.json`:

```json
{
  "context": "lke12345-prod",
  "last_incident_end": "2026-04-25T14:42:11Z",
  "last_incident_duration": "7m12s",
  "last_incident_reason": "recovered",
  "incident_count": 3
}
```

Default location: `$XDG_STATE_HOME/k8s-cluster-health` (or `~/.local/state/k8s-cluster-health` if `XDG_STATE_HOME` is unset). Override with `-state-dir`.

The file is written atomically (`tmp` + rename) at incident close, and loaded at startup so the footer's `last=…` field survives restarts. The cumulative `incident_count` survives too. Context names are sanitised to `[A-Za-z0-9._-]+` for filesystem safety, since LKE / EKS / GKE contexts often contain `:` or `/`.

The full markdown incident reports written to `-log-dir` remain the authoritative record — the state file is just a tiny "what was the last bad thing" pointer for the footer.

### Email alerts via SMTP2GO

Set three things and email alerts are sent on incident open and close:

```sh
export SMTP2GO_API_KEY=api-...
./k8s-cluster-health \
    -context my-cluster \
    -email-from alerts@example.com \
    -email-to oncall@example.com,backup-oncall@example.com
```

The startup banner shows `email=on(2)` or `email=off` so you can tell at a glance whether it's wired up.

- **Cadence.** Open + close (matching `notify-send`); never per-tick.
- **Subject.** `[k8s-cluster-health] <context> ALERT` on open, `[k8s-cluster-health] <context> RECOVERED` on close, `[k8s-cluster-health] <context> SHUTDOWN` if the program is killed mid-incident.
- **Body.** Plain text — header (cluster, API server, start, end, duration, close reason, report path) followed by a per-tick timeline. Same content as the markdown report but flat-text so any client renders it the same.
- **Failures** (HTTP non-2xx, transport error) print a one-line `ERR email send failed: …` to stderr and don't block the poll loop.
- **Shutdown.** A signal-killed session waits for any in-flight email send to complete (up to the SMTP2GO HTTP timeout) before exiting, so the shutdown alert actually leaves the process.

The API key is read from the environment specifically so it doesn't show up in `ps` output or shell history. There's no flag for it.

### Running in a container

A multi-stage `Dockerfile` builds a fully-static binary on top of `gcr.io/distroless/static-debian12`:

```sh
docker build -t k8s-cluster-health .
```

Sample run:

```sh
docker run --rm -it \
  -v "$HOME/.kube:/root/.kube:ro" \
  -v "k8s-cluster-health-state:/state" \
  -e SMTP2GO_API_KEY \
  k8s-cluster-health \
    -context my-cluster \
    -interval 5s \
    -email-from alerts@example.com \
    -email-to oncall@example.com
```

Notes:

- **Kubeconfig.** Mount `~/.kube` (or `$KUBECONFIG`) read-only into `/root/.kube` so the existing default-discovery logic finds it. For an in-cluster pod use a `ServiceAccount` and skip the mount — `client-go` auto-detects in-cluster config.
- **State.** The default entrypoint passes `-log-dir /tmp -state-dir /state`. Mount a named volume at `/state` to keep the per-context "last incident" record across container restarts; `/tmp` for incident reports.
- **Bell + notify-send** are auto-disabled when `/.dockerenv` (or `/run/.containerenv`) is detected — pass `-no-container-detect` if you have a forwarded DBus session and actually want desktop notifications from a container.
- **Footer** auto-disables when stdout isn't a TTY; if you want the footer when running interactively, use `-it` and a terminal that's at least 30×6.

### What it specifically catches

| Signal | Surfaced as |
|---|---|
| `etcd-readiness failed` on managed API | `readyz FAIL [HTTP 500] etcd-readiness failed: …` |
| API server EOF / connection reset | `API ERROR Get "…": EOF` |
| API latency > 1s | `WARN api=NNNNms` |
| API latency > 3s | `ALERT api=NNNNms` |
| Pod restart anywhere in the cluster | `restart ns/pod +N (now M)` |
| Node going `NotReady` | `NotReady node: nodeName` |
| New `Warning` event (Unhealthy, BackOff, FailedMount, etc.) | `event ns/kind/name [Reason] message` |

### What it deliberately does not do

- The runtime working set (restart counters, seen-event set, pending tick buffer) lives only in memory; restart this tool and the watcher re-baselines. The single piece of persisted state is the per-context "last incident" record (cosmetic, footer-only).
- No history / log file; output is append-only on stdout. Pipe it through `tee` if you want a transcript.
- No alerting beyond stdout, terminal bell, `notify-send`, and SMTP2GO email. No Slack, no Pushover, no PagerDuty integrations.
- No CRD / CR-specific health checks — only core API objects (pods, nodes, events) plus `/readyz`.
- No leader election or distributed coordination — this is a single-process diagnostic, not a controller.

## Tests

```sh
go test ./...
go test -v -run . ./...   # verbose
```

The test suite covers the pure / parsing functions:

- `TestSummarizeReadyz` — three cases: all-passing body returned as-is, single failed gate extracted, multiple failed gates joined with `; `.
- `TestStripANSI` — strips colour codes from notification bodies (so the GNOME tray doesn't show raw escape sequences).
- `TestTruncate` and `TestOneLine` — string helpers that bound the size of event messages in notifications and collapse whitespace.
- `TestBuildNotifyBody` — verifies the notification body contains the API summary, NotReady node line, restart deltas, and event entries.
- `TestBuildNotifyBodyTruncatesLargeLists` — checks the `…and N more` trailer when more than 4 deltas/events are passed.
- `TestBuildNotifyBodyEmpty` — the no-details fallback string.
- `TestLastSeen` — preference order `LastTimestamp > EventTime > CreationTimestamp` for event timestamps.
- `TestScanPodsDetectsRestartDelta` — the restart-delta arithmetic used inside `scanPods`.
- `TestPaintNoColorPassthrough` — colour helper passes strings through unchanged when stdout isn't a TTY.
- `TestIncidentOpensOnAlertOnly` — WARN and OK ticks alone don't open an incident; only ALERT does.
- `TestIncidentClosesAfterRecoveryTicks` — incident closes after `recoveryTicks` consecutive OKs, end time is the *first* OK after the last non-OK, duration reflects the unhealthy window, and the report contains the expected aggregates.
- `TestIncidentNonOKResetsRecoveryStreak` — a new ALERT/WARN mid-recovery resets the OK counter; the incident stays open until 3 OKs occur back-to-back.
- `TestForceCloseOnShutdown` — `forceCloseIncident` writes a report with `Closed reason: shutdown` so SIGINT mid-incident still produces a usable artefact.
- `TestDebounceSingleAlertDoesNotOpen` — with `min-confirmations=2`, a single ALERT followed by OK does not open an incident or write a report.
- `TestDebounceTwoConsecutiveAlertsOpen` — two consecutive ALERTs cross the threshold and both ticks land in the incident timeline; start time is the first ALERT, not the confirmation.
- `TestDebounceWarnPreambleIncludedInIncident` — a WARN immediately followed by an ALERT crosses the threshold and the WARN is folded in as preamble (start time is the WARN).
- `TestDebounceWarnOnlySequenceNeverOpens` — a long WARN-only sequence never opens an incident, and the pending buffer stays bounded.
- `TestDebounceAlertResetsOnOK` — two isolated ALERTs separated by an OK are each treated as transient blips; nothing escalates.
- `TestDialAnyEmptyTargetsTreatedAsUp` — with no targets configured, `dialAny` returns `true` (treat as online).
- `TestDialAnyAllUnreachable` — RFC 5737 TEST-NET-1 addresses fail dialing; offline detection works as expected.
- `TestDialAnyOneSucceeds` — a mix of unreachable + a live local listener returns `true` (first success wins).
- `TestLocalNetUpUsesInjectedFunc` and `TestLocalNetUpDefaultsToTrueWithNoTargets` — verify the injectable hook used by tests and the safe default when no check is configured.
- `TestTruncateVisibleNoTruncationNeeded`, `TestTruncateVisibleStripsAnsiFromCount`, `TestTruncateVisibleTruncates`, `TestTruncateVisiblePreservesAnsiButTruncatesText` — the footer's visible-width truncation handles ANSI escapes correctly (escapes don't count toward width; truncation appends a reset + ellipsis so colour doesn't bleed).
- `TestFormatFooterShowsSeverity` — footer renders status, api avg/min/max, pods, nodes, warns, incident count, uptime.
- `TestFormatFooterShowsAvgMinMax` — four successful probes (100, 50, 300, 200 ms) yield `api avg=162ms min=50ms max=300ms`.
- `TestFormatFooterAvgMinMaxIgnoresFailedProbes` — a failed 12-second timeout does NOT pollute avg/min/max.
- `TestFormatFooterApiShowsCurrentValueOnlyBeforeFirstSample` — before any successful probe, the footer shows `api=<latest>ms` with no avg/min/max.
- `TestFormatFooterCountsWarnings` — every WARN-severity tick increments `warns=`.
- `TestFormatFooterWarnCountIncludesUnescalatedAlerts` — pins that ALERT ticks do *not* increment the warn counter (only WARN does).
- `TestFormatFooterShowsActiveIncident` — during an active incident, the footer shows `INCIDENT <elapsed> (N ticks)` instead of the last-incident summary.
- `TestFormatFooterShowsLastIncident` — after closure, the footer shows `last=<YYYY-MM-DD HH:MMZ> (<reason>, <duration>)`.
- `TestSanitizeContextName` — kubeconfig context names are mapped to safe filenames (`:` and `/` become `_`).
- `TestResolveStateDirOverride` / `TestResolveStateDirXDG` — `-state-dir` wins; otherwise `XDG_STATE_HOME` is used.
- `TestStateFileRoundTrip` — `saveState` + `loadState` round-trips a full record; missing file yields zero-value state, not an error.
- `TestParseCSV` — comma-separated parser used by `-email-to` (trims, drops empties).
- `TestEmailerNilWhenMisconfigured` — missing `SMTP2GO_API_KEY`, missing `-email-from`, or empty recipient list yields `nil` (silent disable).
- `TestEmailerSendsExpectedRequest` — uses `httptest.Server` to verify the JSON body, content-type, recipients, sender, and subject sent to SMTP2GO.
- `TestEmailerSendNon2xxReturnsError` — HTTP 401 from SMTP2GO surfaces as an error.
- `TestBuildIncidentEmailBodyOpen` / `TestBuildIncidentEmailBodyClose` — plain-text email body renders header + timeline correctly for both open (no end time, no report path) and close (full bookkeeping).
- `TestCloseIncidentWritesStateFile` — after an incident closes, the per-context JSON state file is written with the correct count, reason, and end time.
- `TestFormatFooterOfflineHidesPodApi` — when offline, the footer suppresses api/pods/nodes (since their values are stale).

The Kubernetes API client itself is not mocked; the live-cluster paths (`scanPods`, `scanNodes`, `scanEvents`, `probeReadyz`) are exercised via the smoke run described below.

### Smoke run

```sh
# Point at any reachable cluster; first tick should land within ~1 second.
./k8s-cluster-health -interval 5s
```

Expected: a config header line, then `OK` ticks every 5 seconds with `api=<ms>`, `pods=N/M`, `nodes=ready`, `new-warn-evt=0`. Use `Ctrl+C` to exit.

To force an `ALERT`, simulate a pod restart:

```sh
# In another terminal, against the watched context:
kubectl delete pod -n kube-system <some-pod-with-restartable-controller>
```

Within one or two ticks you should see a `restart` line and a GNOME notification.

## Design notes

- **Why poll, not watch?** Watches add complexity (event-stream resync, missed-event handling) for no benefit at 10-second granularity. Polling is one HTTP round-trip per resource per tick and gives a clean per-tick severity decision.
- **Why `/readyz?verbose=1` rather than `/healthz`?** `/readyz` returns the failed gate in plain text, and `etcd-readiness failed` is the canonical signal for a flapping LKE control plane. `/healthz` would only tell you the apiserver process is alive, not that etcd behind it is healthy.
- **Why `rest.HTTPClientFor` instead of `RESTClient().Get().AbsPath("/readyz")`?** The `Result.Raw()` path discards bodies on non-2xx, which is exactly when we need the body. A plain `*http.Client` keeps the response body whether the status is 200 or 500.
- **Pod restart deltas, not absolute counts.** Absolute restart counts persist for the pod's whole lifetime; you'd alert forever after the first incident. Deltas reset every poll and only fire when `RestartCount` actually advances.
- **Event dedup key.** `UID + lastTimestamp` rather than `UID` alone — a single event UID can have its `LastTimestamp` advance every minute as identical occurrences are coalesced server-side; we want one notification per advance, not one per UID lifetime.

## Files

- `main.go` — main loop, watcher, footer, incident state machine.
- `state.go` — per-context state file (`<state-dir>/<context>.json`) load/save.
- `email.go` — SMTP2GO email integration + plain-text incident body renderer.
- `main_test.go` — unit tests for pure functions.
- `Dockerfile` / `.dockerignore` — multi-stage build to a distroless static image.
- `go.mod` / `go.sum` — pinned to `k8s.io/client-go v0.31.4`.
- `CLAUDE.md` — guidance for AI-assisted edits to this repo.
