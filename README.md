# k8s-cluster-health

Single-binary Kubernetes cluster health watcher. Polls the API server, pods, nodes, and warning events on an interval, prints a colourised status line per tick, and raises terminal + GNOME desktop notifications when instability is detected.

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

### Sample output

```
k8s-cluster-health context=my-cluster server=https://kube-api.example.local:6443 interval=5s slow=1000ms alert=3000ms notify=on
────────────────────────────────────────────────────────────────────────────────
[14:01:25Z] OK    api=92ms  pods=75/75 nodes=ready new-warn-evt=0
[14:01:30Z] ALERT api=8127ms pods=73/75 nodes=ready new-warn-evt=4
       └── readyz FAIL [HTTP 500] etcd-readiness failed: reason withheld
       └── restart kube-system/calico-kube-controllers-689c764695-8wbx8 +1 (now 13)
       └── event kube-system/pod/calico-kube-controllers-... [Unhealthy] Liveness probe failed: …
       └── event kube-system/pod/coredns-... [FailedCreatePodSandBox] plugin type=calico failed (add): …
[14:01:35Z] ALERT api=ERR Get "https://kube-api.example.local:6443/readyz?verbose=1": EOF
```

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

- No persistent state; restart counters and the seen-event set live only in memory. Restart this tool and you re-baseline.
- No history / log file; output is append-only on stdout. Pipe it through `tee` if you want a transcript.
- No alerting beyond stdout, terminal bell, and `notify-send`. No Slack, no Pushover, no PagerDuty integrations.
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

- `main.go` — the watcher (single file).
- `main_test.go` — unit tests for pure functions.
- `go.mod` / `go.sum` — pinned to `k8s.io/client-go v0.31.4`.
- `CLAUDE.md` — guidance for AI-assisted edits to this repo.
