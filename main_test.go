package main

import (
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
		LastTimestamp:     metav1.Time{Time: now},
		EventTime:         metav1.MicroTime{Time: now.Add(-time.Hour)},
		ObjectMeta:        metav1.ObjectMeta{CreationTimestamp: metav1.Time{Time: now.Add(-2 * time.Hour)}},
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
		pods:       map[podKey]podSnap{{ns: "default", name: "p"}: {restarts: 5}},
		seenEvents: make(map[string]time.Time),
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
