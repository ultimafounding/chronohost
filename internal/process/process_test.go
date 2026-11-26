package process

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type eventRecorder struct {
	mu     sync.Mutex
	events []string
	detail map[string][]string
}

func newEventRecorder() *eventRecorder {
	return &eventRecorder{detail: make(map[string][]string)}
}

func (r *eventRecorder) OnEvent(_ string, event, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	r.detail[event] = append(r.detail[event], detail)
}

func (r *eventRecorder) count(event string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e == event {
			n++
		}
	}
	return n
}

func (r *eventRecorder) details(event string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.detail[event]...)
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func writeScript(t *testing.T, body string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "fake.sh")
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return dir, path
}

func hasLine(lines []Line, want string) bool {
	for _, l := range lines {
		if l.Text == want {
			return true
		}
	}
	return false
}

func TestRingBufferAndANSIStripping(t *testing.T) {
	m := New(nil)
	i := m.ensure("ring")
	raw := "\x1b[31mERROR: temporal rift\x1b[0m"
	i.handleLine(raw, i.gen)

	lines := m.Logs("ring", 1)
	if len(lines) != 1 {
		t.Fatalf("Logs(1) returned %d lines, want 1", len(lines))
	}
	got := lines[0]
	if got.Raw != raw {
		t.Fatalf("Raw = %q, want %q", got.Raw, raw)
	}
	if got.Text != "ERROR: temporal rift" {
		t.Fatalf("Text = %q, want ANSI-free text", got.Text)
	}
	if got.Level != "error" {
		t.Fatalf("Level = %q, want error", got.Level)
	}
	if got.InstanceID != "ring" || got.Time.IsZero() {
		t.Fatalf("line metadata missing: %+v", got)
	}

	// Ring capacity is fixed; asking for more than exists returns everything.
	for n := 0; n < logCapacity+50; n++ {
		i.ring.add(Line{Text: fmt.Sprintf("line-%d", n)})
	}
	all := m.Logs("ring", logCapacity+100)
	if len(all) != logCapacity {
		t.Fatalf("ring length = %d, want %d", len(all), logCapacity)
	}
	if all[0].Text != "line-50" || all[len(all)-1].Text != fmt.Sprintf("line-%d", logCapacity+49) {
		t.Fatalf("ring lost ordering: first=%q last=%q", all[0].Text, all[len(all)-1].Text)
	}
	if last := m.Logs("ring", 3); len(last) != 3 || last[0].Text != fmt.Sprintf("line-%d", logCapacity+47) {
		t.Fatalf("Logs(3) = %+v", last)
	}
}

func TestLineClassification(t *testing.T) {
	tests := []struct {
		raw   string
		level string
	}{
		{"[Chat] Bob: hi", "chat"},
		{"Carol joined the server", "chat"},
		{"Dave left the server", "chat"},
		{"ERROR: disk full", "error"},
		{"System.Exception: boom", "error"},
		{"FATAL crash", "error"},
		{"WARNING: low memory", "warn"},
		{"[WARN] backup slow", "warn"},
		{"ordinary output", "info"},
		{"Dedicated Server now running on Port 42420", "system"},
	}
	for _, tc := range tests {
		if got := classifyLine("id", tc.raw).Level; got != tc.level {
			t.Errorf("classifyLine(%q).Level = %q, want %q", tc.raw, got, tc.level)
		}
	}
}

func TestTelemetryParsingAndEvents(t *testing.T) {
	m := New(nil)
	rec := newEventRecorder()
	m.SetEventHandler(rec)
	i := m.ensure("telemetry")

	lines := []string{
		"Game Version: v1.22.7",
		"Dedicated Server now running on Port 42420",
		"Game day 12",
		"Season: summer",
		"Time: 1234",
		"Temporal storm approaching",
		"[Chat] Alice joined the server",
		"[Chat] Bob joined the server",
		"Charlie left the server",
		"Temporal storm active",
	}
	for _, l := range lines {
		i.handleLine(l, i.gen)
	}
	// Ignore the final explicit count so join/leave tracking is visible.
	i.handleLine("Temporal storm has subsided", i.gen)

	tm := m.Telemetry("telemetry")
	if tm.Version != "v1.22.7" {
		t.Errorf("Version = %q, want v1.22.7", tm.Version)
	}
	if tm.GameDay != 12 {
		t.Errorf("GameDay = %d, want 12", tm.GameDay)
	}
	if tm.Season != "summer" {
		t.Errorf("Season = %q, want summer", tm.Season)
	}
	if tm.Storm != "" {
		t.Errorf("Storm = %q, want empty after subsiding", tm.Storm)
	}
	if tm.Online != 2 || len(tm.Players) != 2 || tm.Players[0] != "Alice" || tm.Players[1] != "Bob" {
		t.Errorf("players = %v, online = %d, want [Alice Bob], 2", tm.Players, tm.Online)
	}
	if got := rec.details("join"); len(got) != 2 || got[0] != "Alice" || got[1] != "Bob" {
		t.Errorf("join details = %v, want [Alice Bob]", got)
	}
	if got := rec.details("leave"); len(got) != 1 || got[0] != "Charlie" {
		t.Errorf("leave details = %v, want [Charlie]", got)
	}

	i.handleLine("Players online: 3", i.gen)
	if got := m.Telemetry("telemetry").Online; got != 3 {
		t.Errorf("Online after explicit count = %d, want 3", got)
	}
	i.handleLine("Backup warning: snapshot failed", i.gen)
	if rec.count("backup-warning") != 1 {
		t.Error("expected one backup-warning event")
	}
}

func TestSubscribeBroadcastAndUnsubscribe(t *testing.T) {
	m := New(nil)
	ch1, unsub1 := m.Subscribe("stream")
	ch2, unsub2 := m.Subscribe("stream")
	i := m.ensure("stream")

	i.handleLine("broadcast", i.gen)
	for name, ch := range map[string]<-chan Line{"ch1": ch1, "ch2": ch2} {
		select {
		case l := <-ch:
			if l.Text != "broadcast" {
				t.Errorf("%s received %q", name, l.Text)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s received nothing", name)
		}
	}

	unsub1()
	unsub1() // duplicate unsubscribe must not panic or double-close
	if _, open := <-ch1; open {
		t.Error("unsubscribed channel should be closed")
	}
	i.handleLine("second", i.gen)
	select {
	case l := <-ch2:
		if l.Text != "second" {
			t.Errorf("ch2 received %q", l.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("ch2 received nothing after unsubscribe of ch1")
	}
	unsub2()
}

func TestLifecycleWithFakeScript(t *testing.T) {
	dir, _ := writeScript(t, `
echo "Game Version: v1.22.7"
printf '\033[32mDedicated Server now running on Port 42420\033[0m\n'
echo "Game day 3"
echo "Season: spring"
echo "[Chat] Alice joined the server"
while IFS= read -r line; do
  echo "got:$line"
  case "$line" in
    /stop) exit 0 ;;
  esac
done
`)
	m := New(nil)
	rec := newEventRecorder()
	m.SetEventHandler(rec)
	spec := Spec{
		ID:          "alpha",
		DataPath:    dir,
		BinaryDir:   dir,
		DLL:         "fake.sh",
		Dotnet:      "/bin/sh",
		StopTimeout: 2 * time.Second,
	}
	if err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Shutdown()
	if !m.Running("alpha") || m.Status("alpha") != "running" {
		t.Fatalf("status = %q, want running", m.Status("alpha"))
	}
	// Start is idempotent while running.
	if err := m.Start(spec); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	waitUntil(t, 3*time.Second, func() bool {
		return hasLine(m.Logs("alpha", 100), "Game Version: v1.22.7")
	}, "startup logs")

	if err := m.Command("alpha", "hello"); err != nil {
		t.Fatalf("Command: %v", err)
	}
	waitUntil(t, 3*time.Second, func() bool {
		return hasLine(m.Logs("alpha", 100), "got:hello")
	}, "command response")

	tm := m.Telemetry("alpha")
	if tm.Version != "v1.22.7" || tm.GameDay != 3 || tm.Season != "spring" || tm.Online != 1 {
		t.Errorf("telemetry = %+v", tm)
	}
	if runtime.GOOS == "linux" && tm.MemoryMB <= 0 {
		t.Errorf("MemoryMB = %d, want positive RSS sample", tm.MemoryMB)
	}
	if tm.Uptime <= 0 {
		t.Errorf("Uptime = %s, want positive", tm.Uptime)
	}

	if err := m.Stop("alpha", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := m.Status("alpha"); got != "stopped" {
		t.Fatalf("status after stop = %q, want stopped", got)
	}
	if rec.count("stop") != 1 || rec.count("crash") != 0 {
		t.Errorf("events = %v, want one stop and no crash", rec.events)
	}
	if err := m.Command("alpha", "hello"); err == nil {
		t.Error("Command on stopped instance should fail")
	}

	// Restart brings the instance back.
	if err := m.Restart(spec); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !m.Running("alpha") {
		t.Fatal("instance not running after Restart")
	}
	if err := m.Stop("alpha", false); err != nil {
		t.Fatalf("force Stop: %v", err)
	}
	if got := m.Status("alpha"); got != "stopped" {
		t.Fatalf("status after force stop = %q", got)
	}
}

func TestGracefulStopDoesNotDeadlock(t *testing.T) {
	_, script := writeScript(t, `exec sleep 30`)
	m := New(nil)
	spec := Spec{
		ID:          "stubborn",
		DLL:         script,
		Dotnet:      "/bin/sh",
		StopTimeout: 150 * time.Millisecond,
	}
	if err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Shutdown()
	start := time.Now()
	if err := m.Stop("stubborn", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Stop took %s, expected prompt escalation", elapsed)
	}
	if got := m.Status("stubborn"); got != "stopped" {
		t.Fatalf("status = %q, want stopped", got)
	}
}

func TestWatchdogRestartsAndIntentionalStopSuppressesIt(t *testing.T) {
	_, script := writeScript(t, `echo boot
exit 7`)
	m := New(nil)
	rec := newEventRecorder()
	m.SetEventHandler(rec)
	spec := Spec{
		ID:         "flappy",
		DLL:        script,
		Dotnet:     "/bin/sh",
		Watchdog:   true,
		MaxRetries: 1,
		Backoff:    10 * time.Millisecond,
	}
	if err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Shutdown()
	waitUntil(t, 3*time.Second, func() bool { return rec.count("start") >= 2 }, "watchdog restart")
	if rec.count("crash") < 1 {
		t.Errorf("crash events = %d, want at least one", rec.count("crash"))
	}
	if err := m.Stop("flappy", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	starts := rec.count("start")
	time.Sleep(100 * time.Millisecond)
	if got := rec.count("start"); got != starts {
		t.Errorf("restart after intentional stop: starts %d -> %d", starts, got)
	}
	if got := m.Status("flappy"); got != "stopped" {
		t.Errorf("status = %q, want stopped", got)
	}
}

func TestStopSuppressesCrashAndWatchdog(t *testing.T) {
	_, script := writeScript(t, `
while IFS= read -r line; do
  case "$line" in
    /stop) exit 3 ;;
  esac
done
`)
	m := New(nil)
	rec := newEventRecorder()
	m.SetEventHandler(rec)
	spec := Spec{
		ID:          "quiet",
		DLL:         script,
		Dotnet:      "/bin/sh",
		Watchdog:    true,
		MaxRetries:  1,
		Backoff:     20 * time.Millisecond,
		StopTimeout: time.Second,
	}
	if err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Shutdown()
	if err := m.Stop("quiet", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if rec.count("crash") != 0 {
		t.Errorf("crash events = %v, want none", rec.details("crash"))
	}
	if rec.count("start") != 1 {
		t.Errorf("start events = %d, want 1", rec.count("start"))
	}
}

func TestStartRejectsMissingBinary(t *testing.T) {
	m := New(nil)
	err := m.Start(Spec{ID: "missing", DLL: "Server.dll", Dotnet: filepath.Join(t.TempDir(), "does-not-exist")})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Start error = %v, want missing binary error", err)
	}
	if got := m.Status("missing"); got != "stopped" {
		t.Errorf("status = %q, want stopped", got)
	}
}

func TestAllAndShutdown(t *testing.T) {
	_, script := writeScript(t, `exec sleep 30`)
	m := New(nil)
	for _, id := range []string{"one", "two"} {
		if err := m.Start(Spec{ID: id, DLL: script, Dotnet: "/bin/sh", StopTimeout: time.Second}); err != nil {
			t.Fatalf("Start(%s): %v", id, err)
		}
	}
	if all := m.All(); len(all) != 2 || all["one"] != "running" || all["two"] != "running" {
		t.Fatalf("All() = %v", all)
	}
	m.Shutdown()
	for id, status := range m.All() {
		if status != "stopped" {
			t.Errorf("%s status = %q, want stopped", id, status)
		}
	}
}
