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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
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

type watcher struct {
	cs         *kubernetes.Clientset
	httpClient *http.Client
	apiHost    string
	cfgContext string

	interval time.Duration
	slowMs   int64
	alertMs  int64
	silent   bool
	notify   bool

	pods        map[podKey]podSnap
	initialized bool
	seenEvents  map[string]time.Time
	prevNotRdy  map[string]bool

	startedAt time.Time
	alertN    int
}

func main() {
	var (
		kubeconfig string
		kubectx    string
		interval   time.Duration
		slowMs     int64
		alertMs    int64
		silent     bool
		noNotify   bool
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (default: $KUBECONFIG or ~/.kube/config).")
	flag.StringVar(&kubectx, "context", "", "Kubeconfig context (default: current-context from kubeconfig).")
	flag.DurationVar(&interval, "interval", 10*time.Second, "Poll interval.")
	flag.Int64Var(&slowMs, "slow-ms", 1000, "API latency threshold for WARN (ms).")
	flag.Int64Var(&alertMs, "alert-ms", 3000, "API latency threshold for ALERT (ms).")
	flag.BoolVar(&silent, "no-bell", false, "Do not ring the terminal bell on alerts.")
	flag.BoolVar(&noNotify, "no-notify", false, "Do not send desktop notifications via notify-send.")
	flag.Parse()

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

	w := &watcher{
		cs:         cs,
		httpClient: hc,
		apiHost:    strings.TrimRight(cfg.Host, "/"),
		cfgContext: ctxName,
		interval:   interval,
		slowMs:     slowMs,
		alertMs:    alertMs,
		silent:     silent,
		notify:     notifyOK,
		pods:       make(map[podKey]podSnap),
		seenEvents: make(map[string]time.Time),
		prevNotRdy: make(map[string]bool),
		startedAt:  time.Now().UTC(),
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
	fmt.Printf("%s context=%s server=%s interval=%s slow=%dms alert=%dms notify=%s\n",
		paint(ansiBold, "k8s-cluster-health"),
		paint(ansiCyan, ctxName),
		paint(ansiDim, w.apiHost),
		interval, slowMs, alertMs, notifyState)
	fmt.Println(strings.Repeat("─", 80))

	w.run(rootCtx)
	fmt.Printf("\n%s exiting after %d alert tick(s).\n", paint(ansiDim, "└──"), w.alertN)
}

func (w *watcher) run(ctx context.Context) {
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

	fmt.Printf("%s %s %s\n",
		paint(ansiDim, "["+stamp+"]"),
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

	if hasAlert && !w.silent && useColor {
		fmt.Print(bell)
	}

	if (hasAlert || hasWarn) && w.notify {
		urgency := "critical"
		title := fmt.Sprintf("k8s-cluster-health [%s] ALERT", w.cfgContext)
		if hasWarn {
			urgency = "normal"
			title = fmt.Sprintf("k8s-cluster-health [%s] WARN", w.cfgContext)
		}
		body := buildNotifyBody(stripANSI(apiAlert), notReady, deltas, evts)
		go sendNotification(title, body, urgency)
	}
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
