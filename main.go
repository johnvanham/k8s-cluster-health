// k8s-cluster-health watches a Kubernetes cluster for control-plane / pod
// instability and prints alerts to the terminal.
//
// Signals it watches:
//   - API server /readyz latency and gate status (catches "etcd-readiness failed"
//     and slow / EOF / context-deadline-exceeded responses).
//   - Pod restart count deltas across the whole cluster.
//   - Node Ready condition.
//   - New Warning events.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
	bell       = "\x07"
)

var useColor = isTerminal(os.Stdout)

func paint(code, s string) string {
	if !useColor {
		return s
	}
	return code + s + ansiReset
}

type podKey struct{ ns, name string }

type podSnap struct {
	restarts int32
}

type restartDelta struct {
	ns, name string
	delta    int32
	total    int32
}

type evtRow struct {
	ns, obj, reason, msg string
	when                 time.Time
}

type readyz struct {
	ok         bool
	statusCode int
	body       string
	err        error
	latency    time.Duration
}

type incidentTick struct {
	when         time.Time
	severity     string // "ALERT" / "WARN" / "OK"
	apiLatencyMs int64
	apiAlert     string // ANSI-stripped summary, may be empty
	notReady     []string
	deltas       []restartDelta
	evts         []evtRow
}

type incident struct {
	startedAt   time.Time
	endedAt     time.Time
	lastNonOKAt time.Time
	firstOKAt   time.Time
	okStreak    int
	ticks       []incidentTick
}

type watcher struct {
	cs         *kubernetes.Clientset
	httpClient *http.Client
	apiHost    string
	cfgContext string

	interval         time.Duration
	slowMs           int64
	alertMs          int64
	silent           bool
	notify           bool
	logDir           string
	recoveryTicks    int
	minConfirmations int
	cur              *incident
	pending          []incidentTick

	netCheckTargets []string
	netCheckFunc    func() bool // injectable for tests; nil falls back to dialAny
	localOnline     bool

	pods        map[podKey]podSnap
	initialized bool
	seenEvents  map[string]time.Time
	prevNotRdy  map[string]bool

	startedAt time.Time
	alertN    int
}

func main() {
	var (
		kubeconfig       string
		kubectx          string
		interval         time.Duration
		slowMs           int64
		alertMs          int64
		silent           bool
		noNotify         bool
		logDir           string
		recoveryTicks    int
		minConfirmations int
		netTargetsCSV    string
		noNetCheck       bool
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (default: $KUBECONFIG or ~/.kube/config).")
	flag.StringVar(&kubectx, "context", "", "Kubeconfig context (default: current-context from kubeconfig).")
	flag.DurationVar(&interval, "interval", 10*time.Second, "Poll interval.")
	flag.Int64Var(&slowMs, "slow-ms", 1000, "API latency threshold for WARN (ms).")
	flag.Int64Var(&alertMs, "alert-ms", 3000, "API latency threshold for ALERT (ms).")
	flag.BoolVar(&silent, "no-bell", false, "Do not ring the terminal bell on alerts.")
	flag.BoolVar(&noNotify, "no-notify", false, "Do not send desktop notifications via notify-send.")
	flag.StringVar(&logDir, "log-dir", ".", "Directory to write incident reports.")
	flag.IntVar(&recoveryTicks, "recovery-ticks", 3, "Consecutive OK ticks required to close an incident.")
	flag.IntVar(&minConfirmations, "min-confirmations", 2, "Consecutive non-OK ticks (with at least one ALERT) required to open an incident.")
	flag.StringVar(&netTargetsCSV, "net-check-targets", "1.1.1.1:443,8.8.8.8:443", "Comma-separated TCP targets used to detect local connectivity loss; first success wins.")
	flag.BoolVar(&noNetCheck, "no-net-check", false, "Disable local-network detection (every API failure becomes a cluster issue).")
	flag.Parse()

	if recoveryTicks < 1 {
		recoveryTicks = 1
	}
	if minConfirmations < 1 {
		minConfirmations = 1
	}

	cfg, ctxName, err := buildConfig(kubeconfig, kubectx)
	if err != nil {
		die("loading kubeconfig: %v", err)
	}
	cfg.Timeout = 15 * time.Second

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		die("building clientset: %v", err)
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		die("building http client: %v", err)
	}

	rootCtx, cancel := signalContext()
	defer cancel()

	notifyOK := !noNotify && notifyAvailable()

	var netTargets []string
	if !noNetCheck {
		for _, t := range strings.Split(netTargetsCSV, ",") {
			if t = strings.TrimSpace(t); t != "" {
				netTargets = append(netTargets, t)
			}
		}
	}

	w := &watcher{
		cs:               cs,
		httpClient:       hc,
		apiHost:          strings.TrimRight(cfg.Host, "/"),
		cfgContext:       ctxName,
		interval:         interval,
		slowMs:           slowMs,
		alertMs:          alertMs,
		silent:           silent,
		notify:           notifyOK,
		logDir:           logDir,
		recoveryTicks:    recoveryTicks,
		minConfirmations: minConfirmations,
		netCheckTargets:  netTargets,
		localOnline:      true,
		pods:             make(map[podKey]podSnap),
		seenEvents:       make(map[string]time.Time),
		prevNotRdy:       make(map[string]bool),
		startedAt:        time.Now().UTC(),
	}

	notifyState := "off"
	switch {
	case noNotify:
		notifyState = "disabled"
	case notifyOK:
		notifyState = "on"
	default:
		notifyState = "unavailable"
	}
	netCheckState := "off"
	if len(netTargets) > 0 {
		netCheckState = strings.Join(netTargets, ",")
	}
	fmt.Printf("%s context=%s server=%s interval=%s slow=%dms alert=%dms confirm=%d recover=%d notify=%s net-check=%s\n",
		paint(ansiBold, "k8s-cluster-health"),
		paint(ansiCyan, ctxName),
		paint(ansiDim, w.apiHost),
		interval, slowMs, alertMs, minConfirmations, recoveryTicks, notifyState, netCheckState)
	fmt.Println(strings.Repeat("─", 80))

	w.run(rootCtx)
	fmt.Printf("\n%s exiting after %d alert tick(s).\n", paint(ansiDim, "└──"), w.alertN)
}

func (w *watcher) run(ctx context.Context) {
	defer w.forceCloseIncident()
	w.tick(ctx) // immediate first tick
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *watcher) tick(ctx context.Context) {
	now := time.Now().UTC()
	stamp := now.Format("15:04:05Z")

	rzCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	rz := w.probeReadyz(rzCtx)
	cancel()

	// Disambiguate API failure vs. local network outage. If the API probe
	// errored and a quick dial to a well-known external host also fails,
	// treat this tick as OFFLINE — render a status line, do NOT escalate,
	// preserve the pending buffer and any open incident as-is. This stops
	// transient local wifi drops from generating bogus cluster alerts.
	if rz.err != nil && (len(w.netCheckTargets) > 0 || w.netCheckFunc != nil) {
		online := w.localNetUp()
		if !online {
			ctxLabel := paint(ansiCyan, w.cfgContext)
			stampLabel := paint(ansiDim, "["+stamp+"]")
			if w.localOnline {
				fmt.Printf("%s %s %s — local network unreachable, pausing checks (%s)\n",
					stampLabel, ctxLabel,
					paint(ansiYellow+ansiBold, "OFFLINE"),
					truncate(rz.err.Error(), 100))
				w.localOnline = false
			} else {
				fmt.Printf("%s %s %s\n",
					stampLabel, ctxLabel,
					paint(ansiDim, "OFFLINE — still no local network"))
			}
			return
		}
	}
	if !w.localOnline {
		fmt.Printf("%s %s %s\n",
			paint(ansiDim, "["+stamp+"]"),
			paint(ansiCyan, w.cfgContext),
			paint(ansiGreen+ansiBold, "ONLINE — local network restored"))
		w.localOnline = true
	}

	podCtx, cancel2 := context.WithTimeout(ctx, 15*time.Second)
	deltas, podSummary, podErr := w.scanPods(podCtx)
	cancel2()

	nodeCtx, cancel3 := context.WithTimeout(ctx, 15*time.Second)
	notReady, nodeErr := w.scanNodes(nodeCtx)
	cancel3()

	evtCtx, cancel4 := context.WithTimeout(ctx, 15*time.Second)
	evts, evtErr := w.scanEvents(evtCtx, now.Add(-2*w.interval))
	cancel4()

	apiAlert := ""
	apiSeverity := 0 // 0=ok,1=warn,2=alert
	switch {
	case rz.err != nil:
		apiAlert = fmt.Sprintf("%s %v", paint(ansiRed+ansiBold, "API ERROR"), rz.err)
		apiSeverity = 2
	case !rz.ok:
		apiAlert = fmt.Sprintf("%s [HTTP %d] %s", paint(ansiRed+ansiBold, "readyz FAIL"),
			rz.statusCode, summarizeReadyz(rz.body))
		apiSeverity = 2
	case rz.latency.Milliseconds() > w.alertMs:
		apiAlert = fmt.Sprintf("%s %dms", paint(ansiRed+ansiBold, "API VERY SLOW"),
			rz.latency.Milliseconds())
		apiSeverity = 2
	case rz.latency.Milliseconds() > w.slowMs:
		apiAlert = fmt.Sprintf("%s %dms", paint(ansiYellow, "API slow"),
			rz.latency.Milliseconds())
		apiSeverity = 1
	}

	bits := []string{fmt.Sprintf("api=%dms", rz.latency.Milliseconds())}
	if podErr == nil {
		bits = append(bits, podSummary)
	} else {
		bits = append(bits, paint(ansiRed, fmt.Sprintf("pods=ERR(%v)", podErr)))
	}
	if nodeErr == nil {
		if len(notReady) == 0 {
			bits = append(bits, "nodes=ready")
		} else {
			bits = append(bits, paint(ansiRed, fmt.Sprintf("nodes-NotReady=%d", len(notReady))))
		}
	} else {
		bits = append(bits, paint(ansiRed, fmt.Sprintf("nodes=ERR(%v)", nodeErr)))
	}
	if evtErr == nil {
		bits = append(bits, fmt.Sprintf("new-warn-evt=%d", len(evts)))
	} else {
		bits = append(bits, paint(ansiRed, fmt.Sprintf("evt=ERR(%v)", evtErr)))
	}

	hasAlert := apiSeverity == 2 ||
		len(deltas) > 0 ||
		len(notReady) > 0 ||
		len(evts) > 0 ||
		podErr != nil || nodeErr != nil || evtErr != nil
	hasWarn := apiSeverity == 1 && !hasAlert

	prefix := paint(ansiGreen+ansiBold, "OK   ")
	switch {
	case hasAlert:
		prefix = paint(ansiRed+ansiBold, "ALERT")
		w.alertN++
	case hasWarn:
		prefix = paint(ansiYellow+ansiBold, "WARN ")
	}

	fmt.Printf("%s %s %s %s\n",
		paint(ansiDim, "["+stamp+"]"),
		paint(ansiCyan, w.cfgContext),
		prefix,
		strings.Join(bits, " "))

	if apiAlert != "" {
		fmt.Printf("       %s %s\n", arrow(), apiAlert)
	}
	for _, n := range notReady {
		fmt.Printf("       %s %s node %s\n", arrow(), paint(ansiRed, "NotReady"), n)
	}
	for _, d := range deltas {
		fmt.Printf("       %s %s %s/%s +%d (now %d)\n",
			arrow(), paint(ansiYellow, "restart"), d.ns, d.name, d.delta, d.total)
	}
	for _, e := range evts {
		fmt.Printf("       %s %s %s/%s [%s] %s\n",
			arrow(), paint(ansiYellow, "event"), e.ns, e.obj, e.reason,
			truncate(oneLine(e.msg), 160))
	}

	severity := "OK"
	switch {
	case hasAlert:
		severity = "ALERT"
	case hasWarn:
		severity = "WARN"
	}
	w.recordIncident(now, severity, stripANSI(apiAlert),
		rz.latency.Milliseconds(), notReady, deltas, evts)
}

// recordIncident drives the incident state machine.
//
// Debounce: non-OK ticks accumulate in w.pending. An incident opens only when
// pending has at least minConfirmations entries AND at least one of them is
// ALERT. WARN-only sequences never open an incident (a sustained slow API is
// visible on screen but won't escalate without a hard failure). An OK tick
// while no incident is open clears pending — a single isolated blip evaporates.
//
// Once open, every subsequent tick (ALERT/WARN/OK) appends to the incident's
// timeline. The incident closes after recoveryTicks consecutive OK ticks; the
// "ended" timestamp is the first OK after the last non-OK so duration reflects
// the unhealthy window, not the recovery probe.
//
// Notification + terminal bell fire once at incident open and once at close
// (no per-tick spam during an open incident).
func (w *watcher) recordIncident(now time.Time, severity, apiAlert string,
	latencyMs int64, notReady []string, deltas []restartDelta, evts []evtRow) {

	mc := w.minConfirmations
	if mc < 1 {
		mc = 1
	}

	isOK := severity == "OK"
	t := incidentTick{
		when:         now,
		severity:     severity,
		apiLatencyMs: latencyMs,
		apiAlert:     apiAlert,
		notReady:     append([]string(nil), notReady...),
		deltas:       append([]restartDelta(nil), deltas...),
		evts:         append([]evtRow(nil), evts...),
	}

	if w.cur != nil {
		// Incident already open; append every tick.
		w.cur.ticks = append(w.cur.ticks, t)
		if isOK {
			w.cur.okStreak++
			if w.cur.firstOKAt.IsZero() {
				w.cur.firstOKAt = now
			}
			if w.cur.okStreak >= w.recoveryTicks {
				w.cur.endedAt = w.cur.firstOKAt
				w.closeIncident("recovered")
			}
			return
		}
		w.cur.lastNonOKAt = now
		w.cur.firstOKAt = time.Time{}
		w.cur.okStreak = 0
		return
	}

	// No incident yet.
	if isOK {
		// Single isolated blip resolved before confirmation; drop the buffer.
		w.pending = nil
		return
	}

	w.pending = append(w.pending, t)

	alerts := 0
	for _, p := range w.pending {
		if p.severity == "ALERT" {
			alerts++
		}
	}
	if alerts == 0 || len(w.pending) < mc {
		// Bound the buffer in case of a long WARN-only stretch that never
		// escalates — keep enough to capture a future ALERT preamble.
		cap := mc * 4
		if cap < 8 {
			cap = 8
		}
		if len(w.pending) > cap {
			w.pending = w.pending[len(w.pending)-cap:]
		}
		return
	}

	// Threshold crossed and we have at least one ALERT in the buffer: escalate.
	w.cur = &incident{
		startedAt:   w.pending[0].when,
		ticks:       append([]incidentTick(nil), w.pending...),
		lastNonOKAt: w.pending[len(w.pending)-1].when,
	}
	pendingCount := len(w.pending)
	last := w.pending[len(w.pending)-1]
	w.pending = nil

	fmt.Printf("       %s %s opened incident at %s (%d confirmation(s))\n",
		arrow(), paint(ansiYellow, "↻"),
		w.cur.startedAt.Format("15:04:05Z"), pendingCount)

	if !w.silent && useColor {
		fmt.Print(bell)
	}
	if w.notify {
		title := fmt.Sprintf("k8s-cluster-health [%s] ALERT", w.cfgContext)
		body := buildNotifyBody(last.apiAlert, last.notReady, last.deltas, last.evts)
		if pendingCount > 1 {
			body = fmt.Sprintf("Confirmed after %d non-OK tick(s).\n%s", pendingCount, body)
		}
		go sendNotification(title, body, "critical")
	}
}

// closeIncident finalises the active incident, writes the markdown report,
// and clears state. reason is included in the closing log line ("recovered"
// or "shutdown").
func (w *watcher) closeIncident(reason string) {
	if w.cur == nil {
		return
	}
	inc := w.cur
	w.cur = nil

	if inc.endedAt.IsZero() {
		if !inc.lastNonOKAt.IsZero() {
			inc.endedAt = inc.lastNonOKAt
		} else if len(inc.ticks) > 0 {
			inc.endedAt = inc.ticks[len(inc.ticks)-1].when
		} else {
			inc.endedAt = time.Now().UTC()
		}
	}

	ts := inc.startedAt.UTC().Format("2006-01-02T15-04-05Z")
	path := filepath.Join(w.logDir, "incident-"+ts+".md")
	if err := writeIncidentReport(path, w.cfgContext, w.apiHost, reason, inc); err != nil {
		fmt.Fprintf(os.Stderr, "%s failed to write incident report %s: %v\n",
			paint(ansiRed, "ERR"), path, err)
		return
	}

	dur := inc.endedAt.Sub(inc.startedAt).Round(time.Second)
	fmt.Printf("       %s %s closed incident (%s) duration=%s report=%s\n",
		arrow(), paint(ansiGreen, "✓"), reason, dur, path)

	if w.notify {
		title := fmt.Sprintf("k8s-cluster-health [%s] %s",
			w.cfgContext, strings.ToUpper(reason))
		body := fmt.Sprintf("Incident closed after %s\nReport: %s", dur, path)
		go sendNotification(title, body, "normal")
	}
}

func (w *watcher) forceCloseIncident() {
	if w.cur != nil {
		w.closeIncident("shutdown")
	}
}

// localNetUp returns true if the local machine appears to have working
// network connectivity. Tests can inject netCheckFunc directly; production
// uses dialAny on netCheckTargets.
func (w *watcher) localNetUp() bool {
	if w.netCheckFunc != nil {
		return w.netCheckFunc()
	}
	if len(w.netCheckTargets) == 0 {
		return true
	}
	return dialAny(w.netCheckTargets, time.Second)
}

// dialAny dials each target concurrently with the given per-dial timeout and
// returns true as soon as one connection succeeds. Goroutines for slower
// targets continue running until their dial completes; the buffered channel
// ensures they never block.
func dialAny(targets []string, perTimeout time.Duration) bool {
	if len(targets) == 0 {
		return true
	}
	ch := make(chan bool, len(targets))
	for _, t := range targets {
		go func(t string) {
			c, err := net.DialTimeout("tcp", t, perTimeout)
			if err != nil {
				ch <- false
				return
			}
			_ = c.Close()
			ch <- true
		}(t)
	}
	for i := 0; i < len(targets); i++ {
		if <-ch {
			return true
		}
	}
	return false
}

func (w *watcher) probeReadyz(ctx context.Context) readyz {
	url := w.apiHost + "/readyz?verbose=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return readyz{err: err}
	}
	start := time.Now()
	resp, err := w.httpClient.Do(req)
	latency := time.Since(start)
	if err != nil {
		return readyz{err: err, latency: latency}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return readyz{
		ok:         resp.StatusCode >= 200 && resp.StatusCode < 300,
		statusCode: resp.StatusCode,
		body:       string(body),
		latency:    latency,
	}
}

func (w *watcher) scanPods(ctx context.Context) ([]restartDelta, string, error) {
	pods, err := w.cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "", err
	}

	var deltas []restartDelta
	var ready, total int
	seen := make(map[podKey]struct{}, len(pods.Items))

	for i := range pods.Items {
		p := &pods.Items[i]
		k := podKey{p.Namespace, p.Name}
		seen[k] = struct{}{}
		total++

		var sumRestarts int32
		allReady := len(p.Status.ContainerStatuses) > 0
		for _, cs := range p.Status.ContainerStatuses {
			sumRestarts += cs.RestartCount
			if !cs.Ready {
				allReady = false
			}
		}
		if allReady && p.Status.Phase == corev1.PodRunning {
			ready++
		}

		prev, had := w.pods[k]
		if w.initialized && had && sumRestarts > prev.restarts {
			deltas = append(deltas, restartDelta{
				ns: p.Namespace, name: p.Name,
				delta: sumRestarts - prev.restarts,
				total: sumRestarts,
			})
		}
		w.pods[k] = podSnap{restarts: sumRestarts}
	}
	for k := range w.pods {
		if _, ok := seen[k]; !ok {
			delete(w.pods, k)
		}
	}
	w.initialized = true
	sort.Slice(deltas, func(i, j int) bool {
		if deltas[i].ns != deltas[j].ns {
			return deltas[i].ns < deltas[j].ns
		}
		return deltas[i].name < deltas[j].name
	})
	return deltas, fmt.Sprintf("pods=%d/%d", ready, total), nil
}

func (w *watcher) scanNodes(ctx context.Context) ([]string, error) {
	nodes, err := w.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var notReady []string
	for i := range nodes.Items {
		n := &nodes.Items[i]
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			notReady = append(notReady, n.Name)
		}
	}
	sort.Strings(notReady)
	return notReady, nil
}

func (w *watcher) scanEvents(ctx context.Context, since time.Time) ([]evtRow, error) {
	evts, err := w.cs.CoreV1().Events(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning",
		Limit:         500,
	})
	if err != nil {
		return nil, err
	}
	var out []evtRow
	for i := range evts.Items {
		e := &evts.Items[i]
		ts := lastSeen(e)
		if ts.Before(since) {
			continue
		}
		// Dedupe by UID; LastTimestamp can advance for repeated events,
		// so combine UID + lastTimestamp to fire once per coalesced burst.
		key := string(e.UID) + "|" + ts.Format(time.RFC3339Nano)
		if _, ok := w.seenEvents[key]; ok {
			continue
		}
		w.seenEvents[key] = ts
		out = append(out, evtRow{
			ns:     e.Namespace,
			obj:    fmt.Sprintf("%s/%s", strings.ToLower(e.InvolvedObject.Kind), e.InvolvedObject.Name),
			reason: e.Reason,
			msg:    e.Message,
			when:   ts,
		})
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	for k, ts := range w.seenEvents {
		if ts.Before(cutoff) {
			delete(w.seenEvents, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].when.Before(out[j].when) })
	return out, nil
}

func lastSeen(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func summarizeReadyz(body string) string {
	var failed []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "[-]") {
			failed = append(failed, strings.TrimSpace(strings.TrimPrefix(line, "[-]")))
		}
	}
	if len(failed) == 0 {
		s := strings.TrimSpace(body)
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		return s
	}
	return strings.Join(failed, "; ")
}

func buildConfig(kubeconfig, kubectx string) (*rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	// Only override the loader's discovery (which already handles KUBECONFIG
	// with colon-separated paths and the ~/.kube/config fallback) when the
	// user explicitly passes -kubeconfig.
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubectx != "" {
		overrides.CurrentContext = kubectx
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	raw, _ := cc.RawConfig()
	name := kubectx
	if name == "" {
		name = raw.CurrentContext
	}
	return cfg, name, nil
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func arrow() string { return paint(ansiDim, "└──") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func notifyAvailable() bool {
	if _, err := exec.LookPath("notify-send"); err != nil {
		return false
	}
	// Heuristic: a graphical session is needed for the notification to surface.
	// notify-send itself will return success even when there's no display, so
	// gate on the usual session-bus / display env vars.
	return os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" ||
		os.Getenv("WAYLAND_DISPLAY") != "" ||
		os.Getenv("DISPLAY") != ""
}

func sendNotification(title, body, urgency string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "notify-send",
		"--app-name=k8s-cluster-health",
		"--urgency="+urgency,
		"--icon=dialog-warning",
		"--category=device.error",
		title, body)
	_ = cmd.Run()
}

func buildNotifyBody(apiSummary string, notReady []string, deltas []restartDelta, evts []evtRow) string {
	var lines []string
	if apiSummary != "" {
		lines = append(lines, apiSummary)
	}
	for _, n := range notReady {
		lines = append(lines, "NotReady node: "+n)
	}
	const maxItems = 4
	if n := len(deltas); n > 0 {
		shown := n
		if shown > maxItems {
			shown = maxItems
		}
		for i := 0; i < shown; i++ {
			d := deltas[i]
			lines = append(lines, fmt.Sprintf("restart %s/%s +%d (now %d)", d.ns, d.name, d.delta, d.total))
		}
		if n > shown {
			lines = append(lines, fmt.Sprintf("…and %d more restart(s)", n-shown))
		}
	}
	if n := len(evts); n > 0 {
		shown := n
		if shown > maxItems {
			shown = maxItems
		}
		for i := 0; i < shown; i++ {
			e := evts[i]
			lines = append(lines, fmt.Sprintf("[%s] %s/%s: %s", e.reason, e.ns, e.obj, truncate(oneLine(e.msg), 100)))
		}
		if n > shown {
			lines = append(lines, fmt.Sprintf("…and %d more event(s)", n-shown))
		}
	}
	if len(lines) == 0 {
		return "(no details)"
	}
	return strings.Join(lines, "\n")
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// writeIncidentReport renders a markdown report of the incident, suitable for
// pasting into a provider support ticket (Linode, AWS, etc.). Sections:
// header, summary aggregates, full per-tick timeline.
func writeIncidentReport(path, ctxName, apiHost, closeReason string, inc *incident) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var nAlert, nWarn, nOK int
	var maxLatency int64
	apiFailures := map[string]int{}
	restartsByPod := map[string]int32{}
	finalCountByPod := map[string]int32{}
	notReadySet := map[string]bool{}
	eventReasons := map[string]string{}
	for _, t := range inc.ticks {
		switch t.severity {
		case "ALERT":
			nAlert++
		case "WARN":
			nWarn++
		case "OK":
			nOK++
		}
		if t.apiLatencyMs > maxLatency {
			maxLatency = t.apiLatencyMs
		}
		if t.apiAlert != "" {
			apiFailures[t.apiAlert]++
		}
		for _, n := range t.notReady {
			notReadySet[n] = true
		}
		for _, d := range t.deltas {
			k := d.ns + "/" + d.name
			restartsByPod[k] += d.delta
			finalCountByPod[k] = d.total
		}
		for _, e := range t.evts {
			if _, ok := eventReasons[e.reason]; !ok {
				eventReasons[e.reason] = e.msg
			}
		}
	}

	dur := inc.endedAt.Sub(inc.startedAt).Round(time.Second)
	fmt.Fprintf(f, "# Cluster instability incident — %s\n\n", inc.startedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "- **Cluster context:** `%s`\n", ctxName)
	fmt.Fprintf(f, "- **API server:** `%s`\n", apiHost)
	fmt.Fprintf(f, "- **Started (UTC):** %s\n", inc.startedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "- **Ended (UTC):** %s\n", inc.endedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "- **Duration:** %s\n", dur)
	fmt.Fprintf(f, "- **Closed reason:** %s\n", closeReason)
	fmt.Fprintf(f, "- **Tool:** k8s-cluster-health (https://github.com/johnvanham/k8s-cluster-health)\n\n")

	fmt.Fprintln(f, "## Summary")
	fmt.Fprintln(f)
	fmt.Fprintf(f, "- Tick severity counts: **%d ALERT**, **%d WARN**, **%d OK** (recovery)\n", nAlert, nWarn, nOK)
	fmt.Fprintf(f, "- Max observed API latency: **%d ms**\n", maxLatency)
	if len(apiFailures) > 0 {
		fmt.Fprintln(f, "- API-server failure signatures observed:")
		for _, k := range sortedKeys(apiFailures) {
			fmt.Fprintf(f, "  - `%s` × %d\n", k, apiFailures[k])
		}
	} else {
		fmt.Fprintln(f, "- API-server failure signatures observed: none")
	}
	if len(restartsByPod) > 0 {
		fmt.Fprintf(f, "- Distinct pods with restart-count increases: **%d**\n", len(restartsByPod))
		for _, k := range sortedKeys(restartsByPod) {
			fmt.Fprintf(f, "  - `%s` +%d (final restart count %d)\n",
				k, restartsByPod[k], finalCountByPod[k])
		}
	}
	if len(notReadySet) > 0 {
		fmt.Fprintf(f, "- Nodes that went NotReady: **%d**\n", len(notReadySet))
		for _, n := range sortedBoolKeys(notReadySet) {
			fmt.Fprintf(f, "  - `%s`\n", n)
		}
	}
	if len(eventReasons) > 0 {
		fmt.Fprintf(f, "- Distinct Warning event reason(s): **%d**\n", len(eventReasons))
		for _, r := range sortedStringKeys(eventReasons) {
			fmt.Fprintf(f, "  - **%s** — first seen: %s\n", r, truncate(oneLine(eventReasons[r]), 240))
		}
	}
	fmt.Fprintln(f)

	fmt.Fprintln(f, "## Timeline")
	fmt.Fprintln(f)
	for _, t := range inc.ticks {
		latency := fmt.Sprintf("%dms", t.apiLatencyMs)
		fmt.Fprintf(f, "### %s — %s (api=%s)\n", t.when.UTC().Format("15:04:05Z"), t.severity, latency)
		if t.apiAlert != "" {
			fmt.Fprintf(f, "- %s\n", t.apiAlert)
		}
		for _, n := range t.notReady {
			fmt.Fprintf(f, "- NotReady node: `%s`\n", n)
		}
		for _, d := range t.deltas {
			fmt.Fprintf(f, "- Pod restart: `%s/%s` +%d (now %d)\n", d.ns, d.name, d.delta, d.total)
		}
		for _, e := range t.evts {
			fmt.Fprintf(f, "- Event: `%s/%s` [%s] %s\n",
				e.ns, e.obj, e.reason, truncate(oneLine(e.msg), 300))
		}
		fmt.Fprintln(f)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedBoolKeys(m map[string]bool) []string   { return sortedKeys(m) }
func sortedStringKeys(m map[string]string) []string { return sortedKeys(m) }
