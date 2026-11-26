package notify_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackm/chronohost/internal/notify"
)

type captured struct {
	method      string
	contentType string
	payload     struct {
		Username string `json:"username"`
		Embeds   []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Color       int    `json:"color"`
			Timestamp   string `json:"timestamp"`
			Footer      struct {
				Text string `json:"text"`
			} `json:"footer"`
		} `json:"embeds"`
	}
}

func server(t *testing.T, status int, body string) (*httptest.Server, func() []captured) {
	t.Helper()
	var requests []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c captured
		c.method = r.Method
		c.contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&c.payload); err != nil {
			t.Errorf("decode webhook payload: %v", err)
		}
		requests = append(requests, c)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []captured { return requests }
}

func TestSendPayload(t *testing.T) {
	srv, requests := server(t, http.StatusNoContent, "")
	when := time.Date(2026, 11, 26, 12, 34, 56, 0, time.FixedZone("UTC+2", 2*3600))
	ev := notify.Event{Instance: "alpha", Type: "backup", Detail: "snapshot created", Time: when}

	// A nil http.Client must fall back to http.DefaultClient.
	n := notify.New(func() (string, bool) { return srv.URL, true }, nil)
	if err := n.Send(context.Background(), ev); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := requests()
	if len(got) != 1 {
		t.Fatalf("got %d requests, want 1", len(got))
	}
	c := got[0]
	if c.method != http.MethodPost || c.contentType != "application/json" {
		t.Errorf("request method/content-type = %s / %q", c.method, c.contentType)
	}
	if c.payload.Username != "ChronoHost" || len(c.payload.Embeds) != 1 {
		t.Fatalf("payload = %+v", c.payload)
	}
	embed := c.payload.Embeds[0]
	if embed.Title != "backup — alpha" || embed.Description != "snapshot created" {
		t.Errorf("embed title/description = %q / %q", embed.Title, embed.Description)
	}
	if embed.Color != 3447003 {
		t.Errorf("color = %d, want 3447003", embed.Color)
	}
	if want := when.UTC().Format(time.RFC3339); embed.Timestamp != want {
		t.Errorf("timestamp = %q, want %q", embed.Timestamp, want)
	}
	if embed.Footer.Text != "ChronoHost" {
		t.Errorf("footer = %+v", embed.Footer)
	}
}

func TestSendColors(t *testing.T) {
	cases := map[string]int{
		"start":   3066993,
		"stop":    15158332,
		"crash":   15158332,
		"backup":  3447003,
		"join":    3066993,
		"leave":   1082739,
		"unknown": 3447003,
	}
	srv, requests := server(t, http.StatusOK, "")
	n := notify.New(func() (string, bool) { return srv.URL, true }, srv.Client())
	for eventType := range cases {
		if err := n.Send(context.Background(), notify.Event{Instance: "alpha", Type: eventType}); err != nil {
			t.Fatalf("Send(%q): %v", eventType, err)
		}
	}
	got := requests()
	if len(got) != len(cases) {
		t.Fatalf("got %d requests, want %d", len(got), len(cases))
	}
	for _, c := range got {
		eventType := strings.Split(c.payload.Embeds[0].Title, " — ")[0]
		if want := cases[eventType]; c.payload.Embeds[0].Color != want {
			t.Errorf("%s color = %d, want %d", eventType, c.payload.Embeds[0].Color, want)
		}
		if c.payload.Embeds[0].Timestamp == "" {
			t.Errorf("missing timestamp for %q", eventType)
		}
	}
}

func TestSendDisabled(t *testing.T) {
	srv, requests := server(t, http.StatusNoContent, "")
	var calls atomic.Int32

	n := notify.New(func() (string, bool) { calls.Add(1); return "", true }, srv.Client())
	if err := n.Send(context.Background(), notify.Event{Type: "start"}); err != nil {
		t.Fatalf("empty URL: %v", err)
	}
	n = notify.New(func() (string, bool) { calls.Add(1); return srv.URL, false }, srv.Client())
	if err := n.Send(context.Background(), notify.Event{Type: "start"}); err != nil {
		t.Fatalf("disabled: %v", err)
	}
	if err := (*notify.Notifier)(nil).Send(context.Background(), notify.Event{}); err != nil {
		t.Fatalf("nil notifier: %v", err)
	}
	if err := notify.New(nil, nil).Send(context.Background(), notify.Event{}); err != nil {
		t.Fatalf("nil getter: %v", err)
	}
	if got := len(requests()); got != 0 {
		t.Errorf("disabled notifier sent %d requests", got)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("getter called %d times, want 2", got)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv, _ := server(t, http.StatusInternalServerError, "webhook exploded")
	n := notify.New(func() (string, bool) { return srv.URL, true }, srv.Client())
	err := n.Send(context.Background(), notify.Event{Type: "crash", Instance: "alpha"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Send with 500 response = %v, want status error", err)
	}
}

func TestSendContextCancelled(t *testing.T) {
	srv, requests := server(t, http.StatusNoContent, "")
	n := notify.New(func() (string, bool) { return srv.URL, true }, srv.Client())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.Send(ctx, notify.Event{Type: "start"}); err == nil {
		t.Fatal("Send with cancelled context returned nil")
	}
	if got := len(requests()); got != 0 {
		t.Errorf("cancelled request reached server: %d", got)
	}
}
