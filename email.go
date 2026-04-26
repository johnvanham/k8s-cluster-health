package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const smtp2goEndpoint = "https://api.smtp2go.com/v3/email/send"

// emailer wraps SMTP2GO's HTTP API. Constructed once at startup and reused;
// nil when the feature is unconfigured (no API key, missing -email-from, or
// no -email-to recipients) — call sites use `if w.email != nil` rather than
// erroring out.
type emailer struct {
	apiKey   string
	from     string
	to       []string
	endpoint string
	client   *http.Client
}

// newEmailer returns nil when email isn't configured. The SMTP2GO_API_KEY
// env var is the credential surface — keeping it out of flags so it doesn't
// appear in `ps` output or shell history.
func newEmailer(from string, to []string) *emailer {
	apiKey := os.Getenv("SMTP2GO_API_KEY")
	if apiKey == "" || from == "" || len(to) == 0 {
		return nil
	}
	return &emailer{
		apiKey:   apiKey,
		from:     from,
		to:       append([]string(nil), to...),
		endpoint: smtp2goEndpoint,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

type smtp2goRequest struct {
	APIKey   string   `json:"api_key"`
	To       []string `json:"to"`
	Sender   string   `json:"sender"`
	Subject  string   `json:"subject"`
	TextBody string   `json:"text_body"`
}

func (e *emailer) send(subject, body string) error {
	if e == nil {
		return nil
	}
	req := smtp2goRequest{
		APIKey:   e.apiKey,
		To:       e.to,
		Sender:   e.from,
		Subject:  subject,
		TextBody: body,
	}
	buf, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("smtp2go HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// buildIncidentEmailBody renders a plain-text body covering an incident,
// usable both at incident-open (inc.endedAt == zero) and incident-close
// (inc.endedAt set). reportPath, when non-empty, points the recipient at
// the on-disk markdown report for the full detail.
func buildIncidentEmailBody(ctxName, apiHost, reason string, inc *incident, reportPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Cluster:    %s\n", ctxName)
	fmt.Fprintf(&b, "API server: %s\n", apiHost)
	fmt.Fprintf(&b, "Started:    %s\n", inc.startedAt.UTC().Format(time.RFC3339))
	if !inc.endedAt.IsZero() {
		dur := inc.endedAt.Sub(inc.startedAt).Round(time.Second)
		fmt.Fprintf(&b, "Ended:      %s\n", inc.endedAt.UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, "Duration:   %s\n", dur)
		fmt.Fprintf(&b, "Closed:     %s\n", reason)
	} else {
		fmt.Fprintln(&b, "Ended:      (still active)")
	}
	if reportPath != "" {
		fmt.Fprintf(&b, "Report:     %s\n", reportPath)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Timeline:")
	for _, t := range inc.ticks {
		fmt.Fprintf(&b, "  %s %-5s api=%dms\n",
			t.when.UTC().Format("15:04:05Z"), t.severity, t.apiLatencyMs)
		if t.apiAlert != "" {
			fmt.Fprintf(&b, "    %s\n", t.apiAlert)
		}
		for _, n := range t.notReady {
			fmt.Fprintf(&b, "    NotReady node: %s\n", n)
		}
		for _, d := range t.deltas {
			fmt.Fprintf(&b, "    restart %s/%s +%d (now %d)\n", d.ns, d.name, d.delta, d.total)
		}
		for _, ev := range t.evts {
			fmt.Fprintf(&b, "    [%s] %s/%s: %s\n",
				ev.reason, ev.ns, ev.obj, truncate(oneLine(ev.msg), 200))
		}
	}
	return b.String()
}

// parseCSV splits a comma-separated value, trimming whitespace and dropping
// empty entries. Used for -email-to and -net-check-targets.
func parseCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
