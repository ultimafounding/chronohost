// Package notify sends ChronoHost lifecycle events to a Discord webhook.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Event describes something that happened to an instance.
type Event struct {
	Instance string    `json:"instance"`
	Type     string    `json:"type"`
	Detail   string    `json:"detail"`
	Time     time.Time `json:"time"`
}

// WebhookGetter returns the configured webhook URL and whether notifications
// are enabled. It is supplied by the caller so settings can be read lazily.
type WebhookGetter func() (url string, enabled bool)

// Notifier delivers events to Discord.
type Notifier struct {
	get WebhookGetter
	hc  *http.Client
}

// New creates a Notifier. A nil getter disables notifications; a nil client
// uses http.DefaultClient.
func New(get WebhookGetter, hc *http.Client) *Notifier {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Notifier{get: get, hc: hc}
}

type webhookPayload struct {
	Username string  `json:"username"`
	Embeds   []embed `json:"embeds"`
}

type embed struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Color       int    `json:"color"`
	Timestamp   string `json:"timestamp"`
	Footer      footer `json:"footer"`
}

type footer struct {
	Text string `json:"text"`
}

// Send posts ev to the configured Discord webhook. Disabled notifications and
// empty URLs are no-ops. Non-2xx responses are reported as errors.
func (n *Notifier) Send(ctx context.Context, ev Event) error {
	if n == nil || n.get == nil {
		return nil
	}
	url, enabled := n.get()
	if !enabled || strings.TrimSpace(url) == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	hc := n.hc
	if hc == nil {
		hc = http.DefaultClient
	}

	when := ev.Time
	if when.IsZero() {
		when = time.Now().UTC()
	}
	payload := webhookPayload{
		Username: "ChronoHost",
		Embeds: []embed{{
			Title:       fmt.Sprintf("%s — %s", ev.Type, ev.Instance),
			Description: ev.Detail,
			Color:       colorFor(ev.Type),
			Timestamp:   when.UTC().Format(time.RFC3339),
			Footer:      footer{Text: "ChronoHost"},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("notify: encode webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: create webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("notify: send webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a bounded amount of the body for diagnostics.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		msg := strings.TrimSpace(string(detail))
		if msg != "" {
			return fmt.Errorf("notify: webhook returned %s: %s", resp.Status, msg)
		}
		return fmt.Errorf("notify: webhook returned %s", resp.Status)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

func colorFor(eventType string) int {
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "start", "join":
		return 3066993
	case "stop", "crash":
		return 15158332
	case "leave":
		return 1082739
	default:
		return 3447003
	}
}
