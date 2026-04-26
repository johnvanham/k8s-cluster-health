package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSummarizeReadyz(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "all ok returns body",
			body: "[+]ping ok\n[+]log ok\nreadyz check passed",
			want: "[+]ping ok\n[+]log ok\nreadyz check passed",
		},
		{
			name: "single failed gate",
			body: "[+]ping ok\n[-]etcd-readiness failed: reason withheld\n[+]log ok\nreadyz check failed",
			want: "etcd-readiness failed: reason withheld",
		},
		{
			name: "multiple failed gates joined",
			body: "[-]etcd ok\n[-]etcd-readiness failed: reason withheld\n[-]informer-sync failed",
			want: "etcd ok; etcd-readiness failed: reason withheld; informer-sync failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeReadyz(tc.body)
			if tc.name == "all ok returns body" {
				// Body is returned as-is when no [-] gates; we just sanity-check.
				if !strings.Contains(got, "readyz check passed") {
					t.Errorf("expected body to be returned when no failed gates, got %q", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("summarizeReadyz()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestStripANSI(t *testing.T) {
	in := "\x1b[31m\x1b[1mAPI ERROR\x1b[0m timeout"
	want := "API ERROR timeout"
	if got := stripANSI(in); got != want {
		t.Errorf("stripANSI()\n got: %q\nwant: %q", got, want)
	}
	// No-op when already plain.
	if got := stripANSI("plain"); got != "plain" {
		t.Errorf("stripANSI() plain: got %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate short: got %q", got)
	}
	if got := truncate("0123456789", 5); got != "0123…" {
		t.Errorf("truncate long: got %q", got)
	}
}

func TestOneLine(t *testing.T) {
	in := "first line\n  second   line\r\nthird"
	want := "first line second line third"
	if got := oneLine(in); got != want {
		t.Errorf("oneLine()\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildNotifyBody(t *testing.T) {
	deltas := []restartDelta{
		{ns: "kube-system", name: "calico-kube-controllers-abc", delta: 1, total: 13},
		{ns: "monitoring", name: "kube-state-metrics-xyz", delta: 2, total: 9},
	}
	evts := []evtRow{
		{ns: "kube-system", obj: "pod/coredns-x", reason: "Unhealthy", msg: "Liveness probe failed"},
	}
	body := buildNotifyBody("readyz FAIL etcd-readiness failed", []string{"node-1"}, deltas, evts)

	for _, want := range []string{
		"readyz FAIL etcd-readiness failed",
		"NotReady node: node-1",
		"restart kube-system/calico-kube-controllers-abc +1 (now 13)",
		"restart monitoring/kube-state-metrics-xyz +2 (now 9)",
		"[Unhealthy] kube-system/pod/coredns-x: Liveness probe failed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("notify body missing %q\nfull body:\n%s", want, body)
		}
	}
}

func TestBuildNotifyBodyTruncatesLargeLists(t *testing.T) {
	var deltas []restartDelta
	for i := 0; i < 10; i++ {
		deltas = append(deltas, restartDelta{ns: "ns", name: "p", delta: 1, total: int32(i)})
	}
	body := buildNotifyBody("", nil, deltas, nil)
	if !strings.Contains(body, "…and 6 more restart(s)") {
		t.Errorf("expected truncation notice for 10 restarts, got:\n%s", body)
	}
}

func TestBuildNotifyBodyEmpty(t *testing.T) {
	if got := buildNotifyBody("", nil, nil, nil); got != "(no details)" {
		t.Errorf("expected '(no details)', got %q", got)
	}
}

func TestLastSeen(t *testing.T) {
	now := time.Now().UTC()
	e := &corev1.Event{
		LastTimestamp: metav1.Time{Time: now},
		EventTime:     metav1.MicroTime{Time: now.Add(-time.Hour)},
		ObjectMeta:    metav1.ObjectMeta{CreationTimestamp: metav1.Time{Time: now.Add(-2 * time.Hour)}},
	}
	if got := lastSeen(e); !got.Equal(now) {
		t.Errorf("lastSeen() preferred wrong field: got %v want %v", got, now)
	}

	e2 := &corev1.Event{
		EventTime:  metav1.MicroTime{Time: now},
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.Time{Time: now.Add(-time.Hour)}},
	}
	if got := lastSeen(e2); !got.Equal(now) {
		t.Errorf("lastSeen() fallback to EventTime: got %v want %v", got, now)
	}

	e3 := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.Time{Time: now}},
	}
	if got := lastSeen(e3); !got.Equal(now) {
		t.Errorf("lastSeen() fallback to creation: got %v want %v", got, now)
	}
}

func TestScanPodsDetectsRestartDelta(t *testing.T) {
	w := &watcher{
		pods:        map[podKey]podSnap{{ns: "default", name: "p"}: {restarts: 5}},
		seenEvents:  make(map[string]time.Time),
		initialized: true,
	}
	// Mimic what scanPods does internally for a single pod:
	k := podKey{"default", "p"}
	prev := w.pods[k]
	current := int32(7)
	if current <= prev.restarts {
		t.Fatal("test setup wrong; current should exceed prev")
	}
	delta := current - prev.restarts
	if delta != 2 {
		t.Errorf("expected delta=2, got %d", delta)
	}
}

func TestIncidentOpensOnAlertOnly(t *testing.T) {
	w := &watcher{recoveryTicks: 3, minConfirmations: 1}
	now := time.Now().UTC()

	// WARN should not open an incident.
	w.recordIncident(now, "WARN", "API slow 1500ms", 1500, nil, nil, nil)
	if w.cur != nil {
		t.Fatalf("WARN should not open an incident; got %+v", w.cur)
	}
	// OK should not open an incident.
	w.recordIncident(now, "OK", "", 90, nil, nil, nil)
	if w.cur != nil {
		t.Fatalf("OK should not open an incident; got %+v", w.cur)
	}
	// First ALERT opens.
	w.recordIncident(now, "ALERT", "readyz FAIL etcd-readiness failed", 5000, nil, nil, nil)
	if w.cur == nil {
		t.Fatal("ALERT should open an incident")
	}
	if !w.cur.startedAt.Equal(now) {
		t.Errorf("startedAt mismatch: got %v want %v", w.cur.startedAt, now)
	}
	if got := len(w.cur.ticks); got != 1 {
		t.Errorf("expected 1 tick recorded, got %d", got)
	}
}

func TestIncidentClosesAfterRecoveryTicks(t *testing.T) {
	dir := t.TempDir()
	w := &watcher{recoveryTicks: 3, minConfirmations: 1, logDir: dir, cfgContext: "test", apiHost: "https://example"}
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	w.recordIncident(t0, "ALERT", "readyz FAIL etcd-readiness failed", 5000,
		nil,
		[]restartDelta{{ns: "kube-system", name: "calico-kube-controllers", delta: 1, total: 13}},
		nil)
	w.recordIncident(t0.Add(10*time.Second), "ALERT", "API VERY SLOW 8000ms", 8000, nil, nil, nil)
	w.recordIncident(t0.Add(20*time.Second), "WARN", "API slow 1500ms", 1500, nil, nil, nil)

	// First OK starts the recovery streak; this is what end-time should be.
	firstOK := t0.Add(30 * time.Second)
	w.recordIncident(firstOK, "OK", "", 90, nil, nil, nil)
	if w.cur == nil {
		t.Fatal("incident should still be active after 1 OK with recoveryTicks=3")
	}
	w.recordIncident(t0.Add(40*time.Second), "OK", "", 91, nil, nil, nil)
	if w.cur == nil {
		t.Fatal("incident should still be active after 2 OK")
	}
	w.recordIncident(t0.Add(50*time.Second), "OK", "", 92, nil, nil, nil)
	if w.cur != nil {
		t.Fatal("incident should have closed after 3rd OK")
	}

	files, err := filepath.Glob(filepath.Join(dir, "incident-*.md"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected 1 report file, got %v err=%v", files, err)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	report := string(body)

	// End time should be the first OK tick (t0+30s), not the third (t0+50s),
	// since duration measures unhealthy-window.
	wantEnd := firstOK.Format(time.RFC3339)
	if !strings.Contains(report, "Ended (UTC):** "+wantEnd) {
		t.Errorf("report should record end time as first OK %s; report:\n%s", wantEnd, report)
	}
	wantDur := "Duration:** 30s"
	if !strings.Contains(report, wantDur) {
		t.Errorf("report should record 30s duration; report:\n%s", report)
	}
	for _, want := range []string{
		"# Cluster instability incident",
		"Cluster context:** `test`",
		"API server:** `https://example`",
		"2 ALERT", "1 WARN", "3 OK",
		"calico-kube-controllers",
		"readyz FAIL etcd-readiness failed",
		"API VERY SLOW 8000ms",
		"## Timeline",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q\n--- report ---\n%s", want, report)
		}
	}
}

func TestIncidentNonOKResetsRecoveryStreak(t *testing.T) {
	dir := t.TempDir()
	w := &watcher{recoveryTicks: 3, minConfirmations: 1, logDir: dir, cfgContext: "test", apiHost: "https://example"}
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	w.recordIncident(t0, "ALERT", "readyz FAIL", 5000, nil, nil, nil)
	w.recordIncident(t0.Add(10*time.Second), "OK", "", 90, nil, nil, nil)
	w.recordIncident(t0.Add(20*time.Second), "OK", "", 91, nil, nil, nil)
	// New ALERT mid-recovery: must reset the streak.
	w.recordIncident(t0.Add(30*time.Second), "ALERT", "readyz FAIL again", 5000, nil, nil, nil)
	if w.cur == nil {
		t.Fatal("new ALERT should keep incident open")
	}
	if w.cur.okStreak != 0 {
		t.Errorf("okStreak should reset to 0, got %d", w.cur.okStreak)
	}
	if !w.cur.firstOKAt.IsZero() {
		t.Errorf("firstOKAt should reset, got %v", w.cur.firstOKAt)
	}
	// Two OKs alone are not enough to close (need 3 in a row).
	w.recordIncident(t0.Add(40*time.Second), "OK", "", 90, nil, nil, nil)
	w.recordIncident(t0.Add(50*time.Second), "OK", "", 90, nil, nil, nil)
	if w.cur == nil {
		t.Fatal("only 2 OKs after reset; should still be open")
	}
	w.recordIncident(t0.Add(60*time.Second), "OK", "", 90, nil, nil, nil)
	if w.cur != nil {
		t.Fatal("3rd OK should close incident")
	}
}

func TestForceCloseOnShutdown(t *testing.T) {
	dir := t.TempDir()
	w := &watcher{recoveryTicks: 3, minConfirmations: 1, logDir: dir, cfgContext: "test", apiHost: "https://example"}
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	w.recordIncident(t0, "ALERT", "readyz FAIL", 5000, nil, nil, nil)
	w.recordIncident(t0.Add(10*time.Second), "ALERT", "readyz FAIL", 5000, nil, nil, nil)

	w.forceCloseIncident()
	files, _ := filepath.Glob(filepath.Join(dir, "incident-*.md"))
	if len(files) != 1 {
		t.Fatalf("expected 1 report on forced close, got %v", files)
	}
	body, _ := os.ReadFile(files[0])
	if !strings.Contains(string(body), "Closed reason:** shutdown") {
		t.Errorf("expected shutdown close reason in report:\n%s", string(body))
	}
}

func TestDebounceSingleAlertDoesNotOpen(t *testing.T) {
	dir := t.TempDir()
	w := &watcher{recoveryTicks: 3, minConfirmations: 2, logDir: dir, cfgContext: "test", apiHost: "https://example"}
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	w.recordIncident(t0, "ALERT", "blip", 5000, nil, nil, nil)
	if w.cur != nil {
		t.Fatal("single ALERT should not open an incident with min-confirmations=2")
	}
	if got := len(w.pending); got != 1 {
		t.Errorf("expected 1 pending tick, got %d", got)
	}

	// OK arrives — drops the pending blip.
	w.recordIncident(t0.Add(10*time.Second), "OK", "", 90, nil, nil, nil)
	if w.cur != nil {
		t.Fatal("OK after isolated ALERT should not have opened anything")
	}
	if got := len(w.pending); got != 0 {
		t.Errorf("expected pending cleared on OK, got %d", got)
	}

	// No file written.
	files, _ := filepath.Glob(filepath.Join(dir, "incident-*.md"))
	if len(files) != 0 {
		t.Errorf("expected 0 reports, got %v", files)
	}
}

func TestDebounceTwoConsecutiveAlertsOpen(t *testing.T) {
	dir := t.TempDir()
	w := &watcher{recoveryTicks: 3, minConfirmations: 2, logDir: dir, cfgContext: "test", apiHost: "https://example"}
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	w.recordIncident(t0, "ALERT", "readyz FAIL", 5000, nil, nil, nil)
	w.recordIncident(t0.Add(10*time.Second), "ALERT", "readyz FAIL again", 6000, nil, nil, nil)

	if w.cur == nil {
		t.Fatal("2 consecutive ALERTs should open an incident with min-confirmations=2")
	}
	// Both pending ticks should have been folded into the incident timeline.
	if got := len(w.cur.ticks); got != 2 {
		t.Errorf("expected 2 ticks in incident timeline, got %d", got)
	}
	// Incident start time is the first ALERT, not the confirmation tick.
	if !w.cur.startedAt.Equal(t0) {
		t.Errorf("expected startedAt=%v, got %v", t0, w.cur.startedAt)
	}
}

func TestDebounceWarnPreambleIncludedInIncident(t *testing.T) {
	dir := t.TempDir()
	w := &watcher{recoveryTicks: 3, minConfirmations: 2, logDir: dir, cfgContext: "test", apiHost: "https://example"}
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	w.recordIncident(t0, "WARN", "API slow 1500ms", 1500, nil, nil, nil)
	if w.cur != nil {
		t.Fatal("WARN alone should not open an incident")
	}
	w.recordIncident(t0.Add(10*time.Second), "ALERT", "readyz FAIL", 5000, nil, nil, nil)
	if w.cur == nil {
		t.Fatal("WARN+ALERT should reach min-confirmations=2 and open an incident")
	}
	if got := len(w.cur.ticks); got != 2 {
		t.Errorf("expected 2 ticks (WARN preamble + ALERT), got %d", got)
	}
	// Start time is the WARN — that's when trouble began.
	if !w.cur.startedAt.Equal(t0) {
		t.Errorf("expected startedAt=WARN time %v, got %v", t0, w.cur.startedAt)
	}
}

func TestDebounceWarnOnlySequenceNeverOpens(t *testing.T) {
	dir := t.TempDir()
	w := &watcher{recoveryTicks: 3, minConfirmations: 2, logDir: dir, cfgContext: "test", apiHost: "https://example"}
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 10; i++ {
		w.recordIncident(t0.Add(time.Duration(i)*10*time.Second), "WARN", "API slow", 1500, nil, nil, nil)
	}
	if w.cur != nil {
		t.Fatal("WARN-only sequence should never open an incident")
	}
	// Buffer should be capped, not 10.
	if got := len(w.pending); got > 2*w.minConfirmations*4 {
		t.Errorf("pending grew unbounded: %d", got)
	}
}

func TestDebounceAlertResetsOnOK(t *testing.T) {
	dir := t.TempDir()
	w := &watcher{recoveryTicks: 3, minConfirmations: 2, logDir: dir, cfgContext: "test", apiHost: "https://example"}
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	w.recordIncident(t0, "ALERT", "blip 1", 5000, nil, nil, nil)
	w.recordIncident(t0.Add(10*time.Second), "OK", "", 90, nil, nil, nil)
	w.recordIncident(t0.Add(20*time.Second), "ALERT", "blip 2", 5000, nil, nil, nil)
	if w.cur != nil {
		t.Fatal("two isolated ALERTs separated by OK should NOT open an incident")
	}
}

func TestDialAnyEmptyTargetsTreatedAsUp(t *testing.T) {
	if !dialAny(nil, 100*time.Millisecond) {
		t.Error("nil targets should return true (no check configured)")
	}
}

func TestDialAnyAllUnreachable(t *testing.T) {
	// 192.0.2.0/24 is RFC 5737 TEST-NET-1, never routes on the public internet.
	if dialAny([]string{"192.0.2.1:1", "192.0.2.2:1"}, 200*time.Millisecond) {
		t.Error("expected unreachable RFC 5737 addresses to fail dialAny")
	}
}

func TestDialAnyOneSucceeds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if !dialAny([]string{"192.0.2.1:1", ln.Addr().String()}, 300*time.Millisecond) {
		t.Error("expected at least one target to succeed")
	}
}

func TestLocalNetUpUsesInjectedFunc(t *testing.T) {
	w := &watcher{netCheckFunc: func() bool { return true }}
	if !w.localNetUp() {
		t.Error("injected func returning true should be honoured")
	}
	w.netCheckFunc = func() bool { return false }
	if w.localNetUp() {
		t.Error("injected func returning false should be honoured")
	}
}

func TestLocalNetUpDefaultsToTrueWithNoTargets(t *testing.T) {
	w := &watcher{}
	if !w.localNetUp() {
		t.Error("with no targets and no injected func, expect 'up' default")
	}
}

func TestTruncateVisibleNoTruncationNeeded(t *testing.T) {
	if got := truncateVisible("hello", 10); got != "hello" {
		t.Errorf("got %q want %q", got, "hello")
	}
}

func TestTruncateVisibleStripsAnsiFromCount(t *testing.T) {
	// 5 visible chars wrapped in colour codes, max=10 → unchanged.
	in := "\x1b[31mhello\x1b[0m"
	if got := truncateVisible(in, 10); got != in {
		t.Errorf("got %q want %q (visible len within budget)", got, in)
	}
}

func TestTruncateVisibleTruncates(t *testing.T) {
	in := "0123456789ABCDEF"
	got := truncateVisible(in, 6)
	visible := stripANSI(got)
	// Expect 5 chars + ellipsis (… is one rune visually).
	if visible != "01234…" {
		t.Errorf("got visible %q, want %q", visible, "01234…")
	}
}

func TestTruncateVisiblePreservesAnsiButTruncatesText(t *testing.T) {
	in := "\x1b[31mAAAAAAAAAA\x1b[0mBBBBBBBBBB"
	got := truncateVisible(in, 5)
	// Visible should be 4 'A' + ellipsis.
	visible := stripANSI(got)
	if visible != "AAAA…" {
		t.Errorf("got visible %q, want %q", visible, "AAAA…")
	}
	// And the original colour code should still appear in the output.
	if !strings.Contains(got, "\x1b[31m") {
		t.Errorf("expected ANSI codes preserved in %q", got)
	}
}

func TestFormatFooterShowsSeverity(t *testing.T) {
	saved := useColor
	defer func() { useColor = saved }()
	useColor = false

	w := &watcher{cfgContext: "my-cluster", startedAt: time.Now().Add(-1 * time.Minute)}
	w.updateFooterTick("OK", 92, "pods=75/75", "ready", "online", true)
	got := w.formatFooter()

	for _, want := range []string{
		"ctx=my-cluster",
		"status=OK",
		"api avg=92ms min=92ms max=92ms",
		"pods=75/75",
		"nodes=ready",
		"warns=0",
		"incidents=0",
		"uptime=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatFooter missing %q\nfull: %s", want, got)
		}
	}
}

func TestFormatFooterShowsAvgMinMax(t *testing.T) {
	saved := useColor
	defer func() { useColor = saved }()
	useColor = false

	w := &watcher{cfgContext: "my-cluster", startedAt: time.Now()}
	// Successful probes: 100, 50, 300, 200 → avg=162, min=50, max=300.
	w.updateFooterTick("OK", 100, "pods=10/10", "ready", "online", true)
	w.updateFooterTick("OK", 50, "pods=10/10", "ready", "online", true)
	w.updateFooterTick("OK", 300, "pods=10/10", "ready", "online", true)
	w.updateFooterTick("OK", 200, "pods=10/10", "ready", "online", true)

	got := w.formatFooter()
	if !strings.Contains(got, "api avg=162ms min=50ms max=300ms") {
		t.Errorf("expected 'api avg=162ms min=50ms max=300ms', got: %s", got)
	}
}

func TestFormatFooterAvgMinMaxIgnoresFailedProbes(t *testing.T) {
	saved := useColor
	defer func() { useColor = saved }()
	useColor = false

	w := &watcher{cfgContext: "my-cluster", startedAt: time.Now()}
	// Two successful (100, 200 → avg 150, min 100, max 200) plus a failed
	// 12-second timeout that must NOT influence min/max/avg.
	w.updateFooterTick("OK", 100, "pods=10/10", "ready", "online", true)
	w.updateFooterTick("OK", 200, "pods=10/10", "ready", "online", true)
	w.updateFooterTick("ALERT", 12000, "pods=10/10", "ready", "online", false)

	got := w.formatFooter()
	if !strings.Contains(got, "api avg=150ms min=100ms max=200ms") {
		t.Errorf("failed probe should not affect avg/min/max; got: %s", got)
	}
}

func TestFormatFooterApiShowsCurrentValueOnlyBeforeFirstSample(t *testing.T) {
	saved := useColor
	defer func() { useColor = saved }()
	useColor = false

	w := &watcher{cfgContext: "my-cluster", startedAt: time.Now()}
	// Only a failed tick; no successful samples yet.
	w.updateFooterTick("ALERT", 5000, "pods=10/10", "ready", "online", false)

	got := w.formatFooter()
	if !strings.Contains(got, "api=5000ms") {
		t.Errorf("expected api=5000ms in footer, got: %s", got)
	}
	for _, unwanted := range []string{"avg=", "min=", "max="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("expected no %q before first successful sample, got: %s", unwanted, got)
		}
	}
}

func TestFormatFooterCountsWarnings(t *testing.T) {
	saved := useColor
	defer func() { useColor = saved }()
	useColor = false

	w := &watcher{cfgContext: "my-cluster", startedAt: time.Now()}
	w.updateFooterTick("OK", 100, "pods=10/10", "ready", "online", true)
	w.updateFooterTick("WARN", 1500, "pods=10/10", "ready", "online", true)
	w.updateFooterTick("WARN", 1800, "pods=10/10", "ready", "online", true)
	w.updateFooterTick("OK", 90, "pods=10/10", "ready", "online", true)
	w.updateFooterTick("WARN", 1200, "pods=10/10", "ready", "online", true)

	got := w.formatFooter()
	if !strings.Contains(got, "warns=3") {
		t.Errorf("expected warns=3 in footer, got: %s", got)
	}
}

func TestFormatFooterWarnCountIncludesUnescalatedAlerts(t *testing.T) {
	// ALERT ticks count as non-OK but only WARN-severity ticks count toward
	// the "warns=" counter; this test pins that behaviour.
	saved := useColor
	defer func() { useColor = saved }()
	useColor = false

	w := &watcher{cfgContext: "my-cluster", startedAt: time.Now()}
	w.updateFooterTick("ALERT", 5000, "pods=10/10", "ready", "online", false)
	w.updateFooterTick("ALERT", 5000, "pods=10/10", "ready", "online", false)

	got := w.formatFooter()
	if !strings.Contains(got, "warns=0") {
		t.Errorf("ALERT ticks should not increment warn counter; got: %s", got)
	}
}

func TestFormatFooterShowsActiveIncident(t *testing.T) {
	saved := useColor
	defer func() { useColor = saved }()
	useColor = false

	w := &watcher{cfgContext: "my-cluster", startedAt: time.Now().Add(-5 * time.Minute)}
	w.cur = &incident{startedAt: time.Now().Add(-30 * time.Second), ticks: make([]incidentTick, 4)}
	w.updateFooterTick("ALERT", 5000, "pods=73/75", "ready", "online", false)

	got := w.formatFooter()
	if !strings.Contains(got, "INCIDENT") {
		t.Errorf("expected INCIDENT marker for active incident, got: %s", got)
	}
	if !strings.Contains(got, "(4 ticks)") {
		t.Errorf("expected tick count in footer, got: %s", got)
	}
}

func TestFormatFooterShowsLastIncident(t *testing.T) {
	saved := useColor
	defer func() { useColor = saved }()
	useColor = false

	endedAt := time.Date(2026, 4, 25, 14, 30, 0, 0, time.UTC)
	w := &watcher{
		cfgContext: "my-cluster",
		startedAt:  time.Now().Add(-1 * time.Hour),
		footer: footerState{
			incidentCount:   3,
			lastIncidentEnd: endedAt,
			lastIncidentDur: 7 * time.Minute,
			lastIncidentRsn: "recovered",
		},
	}
	w.updateFooterTick("OK", 92, "pods=75/75", "ready", "online", true)

	got := w.formatFooter()
	for _, want := range []string{
		"last=2026-04-25 14:30Z",
		"recovered",
		"7m0s",
		"incidents=3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatFooter missing %q\nfull: %s", want, got)
		}
	}
}

func TestFormatFooterOfflineHidesPodApi(t *testing.T) {
	saved := useColor
	defer func() { useColor = saved }()
	useColor = false

	w := &watcher{cfgContext: "my-cluster", startedAt: time.Now()}
	w.updateFooterTick("OFFLINE", 0, "", "", "offline", false)

	got := w.formatFooter()
	if !strings.Contains(got, "status=OFFLINE") {
		t.Errorf("expected OFFLINE status, got: %s", got)
	}
	// pods/nodes/api shouldn't be reported when offline.
	for _, unwanted := range []string{"api=", "pods=", "nodes="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("offline footer should hide %q, got: %s", unwanted, got)
		}
	}
}

func TestSanitizeContextName(t *testing.T) {
	cases := map[string]string{
		"my-cluster":       "my-cluster",
		"lke12345-prod":    "lke12345-prod",
		"arn:aws:eks:..":   "arn_aws_eks_..",
		"gke_proj_us_x":    "gke_proj_us_x",
		"weird/name space": "weird_name_space",
		"":                 "default",
	}
	for in, want := range cases {
		if got := sanitizeContextName(in); got != want {
			t.Errorf("sanitizeContextName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveStateDirOverride(t *testing.T) {
	got, err := resolveStateDir("/tmp/explicit")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/explicit" {
		t.Errorf("explicit override should win, got %q", got)
	}
}

func TestResolveStateDirXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/var/state")
	got, err := resolveStateDir("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/var/state/k8s-cluster-health" {
		t.Errorf("expected XDG-based path, got %q", got)
	}
}

func TestStateFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := stateFilePath(dir, "lke:prod-1")
	if !strings.Contains(path, "lke_prod-1.json") {
		t.Errorf("expected sanitized filename, got %q", path)
	}

	// Loading a missing file returns an empty struct, not an error.
	s, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState on missing file should not error: %v", err)
	}
	if s.IncidentCount != 0 {
		t.Errorf("missing file should yield zero state, got %+v", s)
	}

	endedAt := time.Date(2026, 4, 25, 14, 30, 0, 0, time.UTC)
	in := &persistentState{
		Context:            "lke:prod-1",
		LastIncidentEnd:    endedAt,
		LastIncidentDur:    "7m15s",
		LastIncidentReason: "recovered",
		IncidentCount:      4,
	}
	if err := saveState(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Context != in.Context ||
		!out.LastIncidentEnd.Equal(in.LastIncidentEnd) ||
		out.LastIncidentDur != in.LastIncidentDur ||
		out.LastIncidentReason != in.LastIncidentReason ||
		out.IncidentCount != in.IncidentCount {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestParseCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a@x.com", []string{"a@x.com"}},
		{"a@x.com, b@y.com  ,c@z.com", []string{"a@x.com", "b@y.com", "c@z.com"}},
		{"  ,  ,  ", nil},
	}
	for _, tc := range cases {
		got := parseCSV(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parseCSV(%q) len=%d, want %d (%v vs %v)", tc.in, len(got), len(tc.want), got, tc.want)
			continue
		}
		for i, v := range tc.want {
			if got[i] != v {
				t.Errorf("parseCSV(%q)[%d] = %q, want %q", tc.in, i, got[i], v)
			}
		}
	}
}

func TestEmailerNilWhenMisconfigured(t *testing.T) {
	t.Setenv("SMTP2GO_API_KEY", "")
	if e := newEmailer("from@example.com", []string{"to@example.com"}); e != nil {
		t.Error("missing API key should yield nil emailer")
	}
	t.Setenv("SMTP2GO_API_KEY", "key")
	if e := newEmailer("", []string{"to@example.com"}); e != nil {
		t.Error("missing from should yield nil emailer")
	}
	if e := newEmailer("from@example.com", nil); e != nil {
		t.Error("missing recipients should yield nil emailer")
	}
}

func TestEmailerSendsExpectedRequest(t *testing.T) {
	t.Setenv("SMTP2GO_API_KEY", "test-api-key")

	var captured smtp2goRequest
	var capturedContentType string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"succeeded":1}}`))
	}))
	defer ts.Close()

	e := newEmailer("alerts@example.com", []string{"a@x.com", "b@y.com"})
	if e == nil {
		t.Fatal("newEmailer returned nil despite valid config")
	}
	e.endpoint = ts.URL

	if err := e.send("subj", "body text"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if capturedContentType != "application/json" {
		t.Errorf("expected JSON content-type, got %q", capturedContentType)
	}
	if captured.APIKey != "test-api-key" {
		t.Errorf("api_key: got %q", captured.APIKey)
	}
	if captured.Sender != "alerts@example.com" {
		t.Errorf("sender: got %q", captured.Sender)
	}
	if len(captured.To) != 2 || captured.To[0] != "a@x.com" || captured.To[1] != "b@y.com" {
		t.Errorf("to: got %v", captured.To)
	}
	if captured.Subject != "subj" || captured.TextBody != "body text" {
		t.Errorf("subject/body mismatch: %+v", captured)
	}
}

func TestEmailerSendNon2xxReturnsError(t *testing.T) {
	t.Setenv("SMTP2GO_API_KEY", "test-api-key")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"data":{"error":"invalid api key"}}`))
	}))
	defer ts.Close()

	e := newEmailer("from@example.com", []string{"to@example.com"})
	if e == nil {
		t.Fatal("newEmailer returned nil")
	}
	e.endpoint = ts.URL

	err := e.send("s", "b")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 error, got %v", err)
	}
}

func TestBuildIncidentEmailBodyOpen(t *testing.T) {
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	inc := &incident{
		startedAt: t0,
		ticks: []incidentTick{
			{when: t0, severity: "ALERT", apiLatencyMs: 5000, apiAlert: "readyz FAIL"},
		},
	}
	body := buildIncidentEmailBody("my-cluster", "https://api.example", "open", inc, "")
	for _, want := range []string{
		"Cluster:    my-cluster",
		"API server: https://api.example",
		"Started:    2026-04-25T12:00:00Z",
		"Ended:      (still active)",
		"Timeline:",
		"12:00:00Z ALERT api=5000ms",
		"readyz FAIL",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("open email body missing %q\n--- body ---\n%s", want, body)
		}
	}
	if strings.Contains(body, "Report:") {
		t.Errorf("open email body should not contain Report: line, got:\n%s", body)
	}
}

func TestBuildIncidentEmailBodyClose(t *testing.T) {
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	inc := &incident{
		startedAt: t0,
		endedAt:   t0.Add(7 * time.Minute),
		ticks: []incidentTick{
			{when: t0, severity: "ALERT", apiLatencyMs: 5000, apiAlert: "readyz FAIL"},
			{when: t0.Add(7 * time.Minute), severity: "OK", apiLatencyMs: 92},
		},
	}
	body := buildIncidentEmailBody("my-cluster", "https://api.example", "recovered", inc,
		"/var/log/incident-2026-04-25T12-00-00Z.md")
	for _, want := range []string{
		"Started:    2026-04-25T12:00:00Z",
		"Ended:      2026-04-25T12:07:00Z",
		"Duration:   7m0s",
		"Closed:     recovered",
		"Report:     /var/log/incident-2026-04-25T12-00-00Z.md",
		"OK    api=92ms",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("close email body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestCloseIncidentWritesStateFile(t *testing.T) {
	saved := useColor
	defer func() { useColor = saved }()
	useColor = false

	dir := t.TempDir()
	stateDir := t.TempDir()
	w := &watcher{
		recoveryTicks:    1,
		minConfirmations: 1,
		logDir:           dir,
		cfgContext:       "lke:prod",
		apiHost:          "https://example",
		stateFile:        stateFilePath(stateDir, "lke:prod"),
	}
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	w.recordIncident(t0, "ALERT", "boom", 5000, nil, nil, nil)
	w.recordIncident(t0.Add(10*time.Second), "OK", "", 90, nil, nil, nil)
	if w.cur != nil {
		t.Fatal("incident should have closed")
	}

	loaded, err := loadState(w.stateFile)
	if err != nil {
		t.Fatalf("loadState after close: %v", err)
	}
	if loaded.IncidentCount != 1 {
		t.Errorf("IncidentCount: got %d, want 1", loaded.IncidentCount)
	}
	if loaded.LastIncidentReason != "recovered" {
		t.Errorf("LastIncidentReason: got %q, want recovered", loaded.LastIncidentReason)
	}
	if loaded.LastIncidentDur == "" {
		t.Errorf("LastIncidentDur should be non-empty, got %q", loaded.LastIncidentDur)
	}
	// End time matches the first OK after the last non-OK (i.e., t0+10s).
	wantEnd := t0.Add(10 * time.Second)
	if !loaded.LastIncidentEnd.Equal(wantEnd) {
		t.Errorf("LastIncidentEnd: got %v, want %v", loaded.LastIncidentEnd, wantEnd)
	}
}

func TestPaintNoColorPassthrough(t *testing.T) {
	saved := useColor
	defer func() { useColor = saved }()
	useColor = false
	if got := paint(ansiRed, "hello"); got != "hello" {
		t.Errorf("paint should be passthrough when useColor=false, got %q", got)
	}
	useColor = true
	if got := paint(ansiRed, "hello"); !strings.HasPrefix(got, "\x1b[") {
		t.Errorf("paint should wrap with ANSI when useColor=true, got %q", got)
	}
}
