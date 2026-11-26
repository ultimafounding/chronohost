// Package process supervises Vintage Story dedicated server processes.
//
// A server is launched as `dotnet <dll> --dataPath <dir> [extra args]`. The
// Manager owns one supervisor per instance ID, merging stdout and stderr into
// a single line-oriented log stream, keeping a bounded ring buffer and
// broadcasting lines to subscribers.
package process

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	logCapacity      = 2000
	subscriberBuffer = 256
	defaultStopWait  = 20 * time.Second
	defaultBackoff   = time.Second
	maxBackoff       = 5 * time.Minute
	resetAfter       = 2 * time.Minute
	stdinWriteWait   = 1500 * time.Millisecond
	killWait         = 5 * time.Second
	maxLineSize      = 1024 * 1024
)

// Spec describes how to launch one server instance.
type Spec struct {
	ID          string
	Version     string
	DataPath    string
	BinaryDir   string
	DLL         string
	Dotnet      string // e.g. "dotnet" or "/usr/bin/dotnet"; default "dotnet" if empty
	Port        int
	Watchdog    bool
	MaxRetries  int
	Backoff     time.Duration
	StopTimeout time.Duration
	ExtraArgs   []string
}

// Line is one parsed log line. Text has ANSI escapes stripped; Raw is the
// original line as emitted by the process.
type Line struct {
	InstanceID string    `json:"instanceId"`
	Time       time.Time `json:"time"`
	Level      string    `json:"level"` // info|warn|error|chat|system
	Text       string    `json:"text"`
	Raw        string    `json:"raw"`
}

// Telemetry is a point-in-time snapshot of an instance.
type Telemetry struct {
	Online   int           `json:"online"`
	Players  []string      `json:"players"`
	GameDay  int           `json:"gameDay"`
	Season   string        `json:"season"`
	Storm    string        `json:"storm"` // "" | "active" | "approaching"
	Version  string        `json:"version"`
	Uptime   time.Duration `json:"uptime"`
	CPU      float64       `json:"cpu"`
	MemoryMB int64         `json:"memoryMb"`
}

// EventHandler receives lifecycle and gameplay events. Known events are
// "start", "stop", "crash", "join", "leave" and "backup-warning".
type EventHandler interface {
	OnEvent(instanceID, event, detail string)
}

// Manager supervises all known instances. It is safe for concurrent use.
type Manager struct {
	mu      sync.Mutex
	inst    map[string]*instance
	logf    func(format string, args ...any)
	handler EventHandler
}

type subscriber struct {
	ch chan Line
}

type writeReq struct {
	data string
	done chan error
}

type cpuSample struct {
	valid bool
	pid   int
	at    time.Time
	utime uint64
	stime uint64
}

type telemetryState struct {
	version  string
	gameDay  int
	season   string
	storm    string
	online   int
	players  map[string]struct{}
	gameTime int64
}

type instance struct {
	m  *Manager
	id string

	// mu guards all mutable state below except the ring buffer, which has
	// its own mutex, and the subscriber channels, which are only touched
	// while mu is held.
	mu          sync.Mutex
	spec        Spec
	status      string
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	writes      chan writeReq
	done        chan struct{}
	cancel      chan struct{}
	gen         uint64
	intentional bool
	shutdown    bool
	retries     int
	start       time.Time
	lastStart   time.Time
	ended       time.Time
	prev        cpuSample
	tel         telemetryState

	ring *ringBuffer
	subs map[*subscriber]struct{}
}

type ringBuffer struct {
	mu    sync.Mutex
	buf   []Line
	start int
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{buf: make([]Line, 0, capacity)}
}

func (r *ringBuffer) add(l Line) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) < cap(r.buf) {
		r.buf = append(r.buf, l)
		return
	}
	r.buf[r.start] = l
	r.start = (r.start + 1) % cap(r.buf)
}

func (r *ringBuffer) last(n int) []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.buf) {
		n = len(r.buf)
	}
	out := make([]Line, 0, n)
	for i := 0; i < n; i++ {
		idx := (r.start + len(r.buf) - n + i) % len(r.buf)
		out = append(out, r.buf[idx])
	}
	return out
}

// New creates a Manager. logf may be nil.
func New(logf func(format string, args ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{inst: make(map[string]*instance), logf: logf}
}

// SetEventHandler sets the sink for lifecycle events. Passing nil disables
// events.
func (m *Manager) SetEventHandler(h EventHandler) {
	m.mu.Lock()
	m.handler = h
	m.mu.Unlock()
}

func (m *Manager) emit(id, event, detail string) {
	m.mu.Lock()
	h := m.handler
	m.mu.Unlock()
	if h != nil {
		h.OnEvent(id, event, detail)
	}
}

func (m *Manager) logMessage(format string, args ...any) {
	if m.logf != nil {
		m.logf(format, args...)
	}
}

// lookup returns an existing instance or nil.
func (m *Manager) lookup(id string) *instance {
	m.mu.Lock()
	i := m.inst[id]
	m.mu.Unlock()
	return i
}

// ensure returns the instance for id, creating a stopped placeholder when
// necessary. This lets callers subscribe before Start is called.
func (m *Manager) ensure(id string) *instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i := m.inst[id]; i != nil {
		return i
	}
	i := &instance{
		m:      m,
		id:     id,
		status: "stopped",
		tel:    telemetryState{online: -1, players: make(map[string]struct{})},
		ring:   newRingBuffer(logCapacity),
		subs:   make(map[*subscriber]struct{}),
	}
	m.inst[id] = i
	return i
}

// Start launches spec's instance, or does nothing when it is already running.
func (m *Manager) Start(spec Spec) error {
	if spec.ID == "" {
		return errors.New("process: empty instance ID")
	}
	if err := validateSpec(spec); err != nil {
		return err
	}

	i := m.ensure(spec.ID)
	i.mu.Lock()
	switch i.status {
	case "starting", "running":
		i.mu.Unlock()
		return nil
	case "stopping":
		i.mu.Unlock()
		return fmt.Errorf("process: instance %q is stopping", spec.ID)
	}
	i.cancelWatchdogLocked()
	i.spec = spec
	i.intentional = false
	i.shutdown = false
	i.retries = 0
	i.mu.Unlock()

	return i.spawn(spec)
}

func validateSpec(spec Spec) error {
	dotnet := spec.Dotnet
	if dotnet == "" {
		dotnet = "dotnet"
	}
	if strings.ContainsRune(dotnet, os.PathSeparator) {
		st, err := os.Stat(dotnet)
		if err != nil || st.IsDir() {
			return fmt.Errorf("process: dotnet binary %q not found", dotnet)
		}
	} else if _, err := exec.LookPath(dotnet); err != nil {
		return fmt.Errorf("process: dotnet binary %q not found: %w", dotnet, err)
	}

	if strings.TrimSpace(spec.DLL) == "" {
		return errors.New("process: DLL must not be empty")
	}
	// A leading '-' means the "DLL" is actually a launcher flag (useful for
	// tests using sh -c); there is no file to check in that case.
	if !strings.HasPrefix(spec.DLL, "-") {
		path := spec.DLL
		if !filepath.IsAbs(path) && spec.BinaryDir != "" {
			path = filepath.Join(spec.BinaryDir, path)
		}
		if spec.BinaryDir != "" || strings.ContainsRune(path, os.PathSeparator) {
			if st, err := os.Stat(path); err != nil || st.IsDir() {
				return fmt.Errorf("process: server DLL %q not found", path)
			}
		}
	}
	return nil
}

func commandArgs(spec Spec) []string {
	dll := spec.DLL
	if !strings.HasPrefix(dll, "-") && spec.BinaryDir != "" && !filepath.IsAbs(dll) {
		dll = filepath.Join(spec.BinaryDir, dll)
	}
	var args []string
	if strings.HasPrefix(dll, "-") {
		// Let flags consume their arguments before our own flags, e.g.
		// sh -c 'script' --dataPath path.
		args = append(args, dll)
		args = append(args, spec.ExtraArgs...)
	} else {
		args = append(args, dll)
	}
	if spec.DataPath != "" {
		args = append(args, "--dataPath", spec.DataPath)
	}
	if !strings.HasPrefix(dll, "-") {
		args = append(args, spec.ExtraArgs...)
	}
	return args
}

// spawn starts one process incarnation. It holds the instance lock across
// exec.Start so a concurrent Stop can never touch a half-started process.
func (i *instance) spawn(spec Spec) error {
	dotnet := spec.Dotnet
	if dotnet == "" {
		dotnet = "dotnet"
	}
	args := commandArgs(spec)

	i.mu.Lock()
	if i.intentional || i.shutdown {
		i.mu.Unlock()
		return errors.New("process: intentional stop in progress")
	}

	cmd := exec.Command(dotnet, args...)
	// Vintage Story resolves relative paths in serverconfig.json (notably
	// Saves/default.vcdbs) from its working directory. Run inside the instance
	// data path so saves, configs and mods stay with their world.
	if spec.DataPath != "" {
		cmd.Dir = spec.DataPath
	}
	// Give stdout and stderr the same OS pipe. Assigning an *os.File avoids
	// cmd.Wait waiting on copy goroutines, which could otherwise hang if a
	// grandchild inherits the write end.
	pr, pw, err := os.Pipe()
	if err != nil {
		i.mu.Unlock()
		return fmt.Errorf("process: stdout pipe: %w", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	newWrites := make(chan writeReq, 64)
	newDone := make(chan struct{})
	stdin, err := cmd.StdinPipe()
	if err != nil {
		pw.Close()
		pr.Close()
		i.mu.Unlock()
		return fmt.Errorf("process: stdin pipe: %w", err)
	}
	i.status = "starting"
	if err := cmd.Start(); err != nil {
		stdin.Close()
		pw.Close()
		pr.Close()
		i.status = "crashed"
		i.cmd = nil
		i.stdin = nil
		i.mu.Unlock()
		close(newDone)
		i.m.emit(i.id, "crash", err.Error())
		return fmt.Errorf("process: start instance %q: %w", i.id, err)
	}
	pw.Close() // only the child should hold the write end

	gen := i.gen + 1
	i.gen = gen
	i.cmd = cmd
	i.stdin = stdin
	i.writes = newWrites
	i.done = newDone
	i.cancel = nil
	i.status = "running"
	i.start = time.Now()
	i.lastStart = i.start
	i.ended = time.Time{}
	i.prev = cpuSample{}
	i.tel = telemetryState{version: spec.Version, online: -1, players: make(map[string]struct{})}

	writes, done := i.writes, i.done
	pid := cmd.Process.Pid
	i.mu.Unlock()

	i.m.logMessage("process: started %q pid=%d", i.id, pid)
	i.m.emit(i.id, "start", "")
	scanDone := make(chan struct{})
	go i.writerLoop(stdin, writes, done)
	go i.scanLoop(pr, gen, scanDone)
	go i.waitLoop(cmd, pr, gen, done, scanDone)
	return nil
}

func (i *instance) waitLoop(cmd *exec.Cmd, out *os.File, gen uint64, done, scanDone chan struct{}) {
	runErr := cmd.Wait()
	// Normally the scanner sees EOF as soon as the child exits. If a
	// grandchild keeps the pipe open, force it closed after a short drain.
	select {
	case <-scanDone:
	case <-time.After(250 * time.Millisecond):
		out.Close()
		select {
		case <-scanDone:
		case <-time.After(100 * time.Millisecond):
		}
	}
	i.handleExit(gen, runErr, done)
}

func (i *instance) handleExit(gen uint64, runErr error, done chan struct{}) {
	i.mu.Lock()
	if i.gen != gen {
		i.mu.Unlock()
		close(done)
		return
	}
	intentional := i.intentional || i.shutdown
	spec := i.spec
	i.cmd = nil
	i.stdin = nil
	i.writes = nil
	i.ended = time.Now()
	if intentional {
		i.status = "stopped"
	} else {
		i.status = "crashed"
	}
	i.mu.Unlock()

	if intentional {
		i.m.logMessage("process: stopped %q", i.id)
		i.m.emit(i.id, "stop", "")
	} else {
		detail := ""
		if runErr != nil {
			detail = runErr.Error()
		}
		i.m.logMessage("process: %q exited unexpectedly: %s", i.id, detail)
		i.m.emit(i.id, "crash", detail)
	}
	close(done)

	if !intentional && spec.Watchdog {
		i.retryOnce(gen, spec)
	}
}

func (i *instance) cancelWatchdogLocked() {
	if i.cancel != nil {
		close(i.cancel)
		i.cancel = nil
	}
}

// retryOnce schedules a single restart after exponential backoff. Each new
// crash creates another retryOnce, so retries is a cumulative counter.
func (i *instance) retryOnce(gen uint64, spec Spec) {
	max := spec.MaxRetries
	if max < 0 {
		max = 0
	} else if max == 0 {
		max = 3
	}
	backoff := spec.Backoff
	if backoff <= 0 {
		backoff = defaultBackoff
	}

	i.mu.Lock()
	if i.gen != gen || i.intentional || i.shutdown {
		i.mu.Unlock()
		return
	}
	if time.Since(i.lastStart) > resetAfter {
		i.retries = 0
	}
	if i.retries >= max {
		i.mu.Unlock()
		return
	}
	i.retries++
	attempt := i.retries
	cancel := make(chan struct{})
	i.cancel = cancel
	i.mu.Unlock()

	delay := backoff
	for n := 1; n < attempt && delay < maxBackoff/2; n++ {
		delay *= 2
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}
	timer := time.NewTimer(delay)
	select {
	case <-cancel:
		timer.Stop()
		return
	case <-timer.C:
	}

	i.mu.Lock()
	stale := i.gen != gen || i.intentional || i.shutdown || i.cancel != cancel
	i.mu.Unlock()
	if stale {
		return
	}
	i.m.logMessage("process: restarting %q after %s (attempt %d)", i.id, delay, attempt)
	if err := i.spawn(spec); err != nil {
		i.m.logMessage("process: restart of %q failed: %v", i.id, err)
	}
}

// Stop stops an instance. Graceful stops write /stop to stdin, wait for a
// clean exit, then escalate to SIGTERM and finally SIGKILL. An intentional
// stop always suppresses the watchdog and crash events.
func (m *Manager) Stop(id string, graceful bool) error {
	i := m.lookup(id)
	if i == nil {
		return nil
	}

	i.mu.Lock()
	i.intentional = true
	i.cancelWatchdogLocked()
	status := i.status
	cmd, stdin, writes, done := i.cmd, i.stdin, i.writes, i.done
	timeout := i.spec.StopTimeout
	i.mu.Unlock()

	if status == "stopped" || status == "crashed" || done == nil {
		if status == "crashed" {
			// An explicit Stop takes ownership of an already-crashed
			// instance: no further restarts, and the status is settled.
			i.mu.Lock()
			i.status = "stopped"
			i.mu.Unlock()
		}
		return nil
	}
	if status == "stopping" {
		select {
		case <-done:
			return nil
		case <-time.After(killWait):
			return fmt.Errorf("process: timeout waiting for %q to stop", id)
		}
	}

	i.mu.Lock()
	if i.status != "stopping" {
		i.status = "stopping"
	}
	i.mu.Unlock()

	if !graceful {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return waitClosed(done, killWait, id)
	}

	if writes != nil && stdin != nil {
		_ = sendWrite(writes, "/stop\n", stdinWriteWait)
	}
	if timeout <= 0 {
		timeout = defaultStopWait
	}
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
	}

	if cmd != nil && cmd.Process != nil {
		_ = terminate(cmd.Process)
	}
	select {
	case <-done:
		return nil
	case <-time.After(killWait):
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return waitClosed(done, killWait, id)
}

func waitClosed(done <-chan struct{}, timeout time.Duration, id string) error {
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("process: timeout stopping %q", id)
	}
}

func sendWrite(ch chan<- writeReq, data string, timeout time.Duration) error {
	if ch == nil {
		return errors.New("process: no stdin")
	}
	req := writeReq{data: data, done: make(chan error, 1)}
	enqueueTimer := time.NewTimer(timeout)
	select {
	case ch <- req:
		enqueueTimer.Stop()
	case <-enqueueTimer.C:
		return errors.New("process: timeout queueing stdin write")
	}
	writeTimer := time.NewTimer(timeout)
	defer writeTimer.Stop()
	select {
	case err := <-req.done:
		return err
	case <-writeTimer.C:
		return errors.New("process: timeout writing to stdin")
	}
}

// writerLoop serializes stdin writes. A process that never reads stdin can
// block the write, but callers use a timeout and the pipe is closed when the
// process exits, so no caller ever blocks indefinitely.
func (i *instance) writerLoop(stdin io.WriteCloser, writes <-chan writeReq, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case req := <-writes:
			_, err := io.WriteString(stdin, req.data)
			select {
			case req.done <- err:
			default:
			}
			if err != nil {
				return
			}
		}
	}
}

// Restart stops then starts an instance.
func (m *Manager) Restart(spec Spec) error {
	if err := m.Stop(spec.ID, true); err != nil {
		return err
	}
	return m.Start(spec)
}

// Running reports whether id is currently running.
func (m *Manager) Running(id string) bool {
	return m.Status(id) == "running"
}

// Status returns stopped|starting|running|stopping|crashed for id.
func (m *Manager) Status(id string) string {
	i := m.lookup(id)
	if i == nil {
		return "stopped"
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status
}

// Command writes command plus a newline to the instance's stdin.
func (m *Manager) Command(id, command string) error {
	i := m.lookup(id)
	if i == nil {
		return fmt.Errorf("process: unknown instance %q", id)
	}
	i.mu.Lock()
	running := i.status == "running"
	writes := i.writes
	i.mu.Unlock()
	if !running {
		return fmt.Errorf("process: instance %q is not running", id)
	}
	return sendWrite(writes, command+"\n", stdinWriteWait)
}

// Logs returns the last n log lines for id from the ring buffer.
func (m *Manager) Logs(id string, n int) []Line {
	i := m.lookup(id)
	if i == nil {
		return []Line{}
	}
	return i.ring.last(n)
}

// Subscribe returns a buffered channel receiving new log lines and an
// unsubscribe function. Broadcasts are non-blocking; a slow subscriber may
// miss lines. Unsubscribing twice is harmless.
func (m *Manager) Subscribe(id string) (<-chan Line, func()) {
	i := m.ensure(id)
	s := &subscriber{ch: make(chan Line, subscriberBuffer)}
	i.mu.Lock()
	i.subs[s] = struct{}{}
	i.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			i.mu.Lock()
			if _, ok := i.subs[s]; ok {
				delete(i.subs, s)
				close(s.ch)
			}
			i.mu.Unlock()
		})
	}
	return s.ch, unsub
}

// All returns the status of every known instance keyed by ID.
func (m *Manager) All() map[string]string {
	m.mu.Lock()
	ids := make([]string, 0, len(m.inst))
	for id := range m.inst {
		ids = append(ids, id)
	}
	out := make(map[string]string, len(ids))
	m.mu.Unlock()
	for _, id := range ids {
		out[id] = m.Status(id)
	}
	return out
}

// Shutdown intentionally stops all instances and disables their watchdogs.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	all := make([]*instance, 0, len(m.inst))
	for _, i := range m.inst {
		all = append(all, i)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, i := range all {
		i.mu.Lock()
		i.shutdown = true
		i.cancelWatchdogLocked()
		i.mu.Unlock()
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_ = m.Stop(id, true)
		}(i.id)
	}
	wg.Wait()
}

// Telemetry returns a snapshot of the instance's current telemetry, including
// on-demand CPU and RSS samples from /proc on Linux.
func (m *Manager) Telemetry(id string) Telemetry {
	i := m.lookup(id)
	if i == nil {
		return Telemetry{}
	}

	i.mu.Lock()
	status := i.status
	pid := 0
	if i.cmd != nil && i.cmd.Process != nil {
		pid = i.cmd.Process.Pid
	}
	gen, prev := i.gen, i.prev
	start, ended := i.start, i.ended
	version, gameDay, season, storm := i.tel.version, i.tel.gameDay, i.tel.season, i.tel.storm
	online := i.tel.online
	players := make([]string, 0, len(i.tel.players))
	for name := range i.tel.players {
		players = append(players, name)
	}
	i.mu.Unlock()

	sort.Strings(players)
	if online < 0 {
		online = len(players)
	}
	tm := Telemetry{
		Online:   online,
		Players:  players,
		GameDay:  gameDay,
		Season:   season,
		Storm:    storm,
		Version:  version,
		CPU:      0,
		MemoryMB: 0,
	}
	if !start.IsZero() {
		end := ended
		if end.IsZero() || status == "running" || status == "starting" {
			end = time.Now()
		}
		if end.After(start) {
			tm.Uptime = end.Sub(start)
		}
	}
	if pid > 0 {
		cpu, mem, sample := sampleProcess(pid, prev)
		tm.CPU, tm.MemoryMB = cpu, mem
		if sample.valid {
			i.mu.Lock()
			if i.gen == gen && pidOf(i) == pid {
				i.prev = sample
			}
			i.mu.Unlock()
		}
	}
	return tm
}

// pidOf must be called with i.mu held.
func pidOf(i *instance) int {
	if i.cmd != nil && i.cmd.Process != nil {
		return i.cmd.Process.Pid
	}
	return 0
}

var (
	ansiRE       = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07]*(?:\x07|\x1b\\\\)")
	versionRE    = regexp.MustCompile(`(?i)\bGame\s+Version\s*:\s*(v?[0-9][0-9A-Za-z._-]*)`)
	gameDayRE    = regexp.MustCompile(`(?i)\bGame\s*day\s*[:#]?\s*(\d+)`)
	seasonRE     = regexp.MustCompile(`(?i)\bSeason\s*:\s*([A-Za-z]+)`)
	gameTimeRE   = regexp.MustCompile(`(?i)\bTime\s*:\s*(\d+)`)
	onlineRE     = regexp.MustCompile(`(?i)\b(?:online\s+players|players\s+online|online)\s*[:=]?\s*(\d+)\b`)
	joinRE       = regexp.MustCompile(`(?i)\bjoined\s+(?:the\s+)?server\b`)
	leaveRE      = regexp.MustCompile(`(?i)\b(?:left\s+(?:the\s+)?server|disconnected)\b`)
	runningRE    = regexp.MustCompile(`(?i)Dedicated\s+Server\s+now\s+running\s+on\s+Port`)
	backupWarnRE = regexp.MustCompile(`(?i)(?:backup[^\n]{0,80}(?:warn|fail|error)|(?:warn|fail|error)[^\n]{0,80}backup)`)
)

func stripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

func classifyLine(id, raw string) Line {
	text := stripANSI(raw)
	low := strings.ToLower(text)
	l := Line{InstanceID: id, Time: time.Now(), Level: "info", Text: text, Raw: raw}
	switch {
	case strings.Contains(low, "[chat]") || joinRE.MatchString(text) || leaveRE.MatchString(text):
		l.Level = "chat"
	case strings.Contains(low, "error") || strings.Contains(low, "exception") || strings.Contains(low, "fatal"):
		l.Level = "error"
	case strings.Contains(low, "warn"):
		l.Level = "warn"
	case runningRE.MatchString(text) || versionRE.MatchString(text):
		l.Level = "system"
	}
	return l
}

// parsePlayer extracts the name just before the join/leave phrase.
func parsePlayer(text string) string {
	clean := stripANSI(text)
	clean = strings.ReplaceAll(clean, "[Chat]", "")
	for _, re := range []*regexp.Regexp{joinRE, leaveRE} {
		loc := re.FindStringIndex(clean)
		if loc == nil {
			continue
		}
		fields := strings.Fields(clean[:loc[0]])
		if len(fields) == 0 {
			return ""
		}
		name := strings.Trim(fields[len(fields)-1], "[]()<>.,:;!\"'")
		if len(name) > 64 {
			return ""
		}
		return name
	}
	return ""
}

func (i *instance) scanLoop(pr *os.File, gen uint64, scanDone chan struct{}) {
	defer close(scanDone)
	defer pr.Close()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), maxLineSize)
	for sc.Scan() {
		i.handleLine(sc.Text(), gen)
	}
}

func (i *instance) handleLine(raw string, gen uint64) {
	line := classifyLine(i.id, raw)
	i.ring.add(line)
	i.parseTelemetry(line.Text, gen)

	joined := joinRE.MatchString(line.Text)
	left := leaveRE.MatchString(line.Text)
	player := parsePlayer(line.Text)
	if joined {
		i.m.emit(i.id, "join", player)
	} else if left {
		i.m.emit(i.id, "leave", player)
	}
	if backupWarnRE.MatchString(line.Text) {
		i.m.emit(i.id, "backup-warning", line.Text)
	}

	i.mu.Lock()
	subs := make([]*subscriber, 0, len(i.subs))
	for s := range i.subs {
		subs = append(subs, s)
	}
	i.mu.Unlock()
	// Copy the subscriber set under the lock, then broadcast: a subscriber
	// can only be removed (and its channel closed) by unsubscribe, which
	// also holds the lock. To avoid a send on a closed channel, re-check
	// membership while locked for each send below.
	for _, s := range subs {
		i.mu.Lock()
		_, ok := i.subs[s]
		if ok {
			select {
			case s.ch <- line:
			default:
			}
		}
		i.mu.Unlock()
	}
}

// parseTelemetry updates telemetry from one line. Failures are ignored.
func (i *instance) parseTelemetry(text string, gen uint64) {
	low := strings.ToLower(text)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.gen != gen {
		return
	}
	if m := versionRE.FindStringSubmatch(text); len(m) == 2 {
		i.tel.version = m[1]
	}
	if runningRE.MatchString(text) && i.status == "starting" {
		i.status = "running"
	}
	if m := gameDayRE.FindStringSubmatch(text); len(m) == 2 {
		if day, err := strconv.Atoi(m[1]); err == nil {
			i.tel.gameDay = day
		}
	}
	if m := gameTimeRE.FindStringSubmatch(text); len(m) == 2 {
		if t, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			i.tel.gameTime = t
		}
	}
	if m := seasonRE.FindStringSubmatch(text); len(m) == 2 {
		i.tel.season = strings.ToLower(m[1])
	}
	if m := onlineRE.FindStringSubmatch(text); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			i.tel.online = n
		}
	}
	if strings.Contains(low, "temporal storm") || strings.Contains(low, "storm") {
		switch {
		case strings.Contains(low, "approach"), strings.Contains(low, "imminent"):
			i.tel.storm = "approaching"
		case strings.Contains(low, "subside"), strings.Contains(low, "end"),
			strings.Contains(low, "clear"), strings.Contains(low, "calm"):
			i.tel.storm = ""
		case strings.Contains(low, "active"), strings.Contains(low, "begin"),
			strings.Contains(low, "start"), strings.Contains(low, "rage"):
			i.tel.storm = "active"
		}
	}

	if joined := joinRE.MatchString(text); joined {
		player := parsePlayer(text)
		if player == "" {
			i.tel.online++
		} else if _, exists := i.tel.players[player]; !exists {
			i.tel.players[player] = struct{}{}
			if i.tel.online < 0 {
				i.tel.online = 0
			}
			i.tel.online++
		}
	}
	if left := leaveRE.MatchString(text); left {
		player := parsePlayer(text)
		known := player == ""
		if player != "" {
			_, known = i.tel.players[player]
			delete(i.tel.players, player)
		}
		if known && i.tel.online > 0 {
			i.tel.online--
		}
	}
}

// sampleProcess samples utime/stime and VmRSS for pid. It returns zeros
// whenever /proc is unavailable (non-Linux, missing process, parse failure).
func sampleProcess(pid int, prev cpuSample) (cpu float64, memMB int64, sample cpuSample) {
	if runtime.GOOS != "linux" || pid <= 0 {
		return 0, 0, cpuSample{}
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, cpuSample{}
	}
	closeIdx := bytes.LastIndexByte(stat, ')')
	if closeIdx < 0 {
		return 0, 0, cpuSample{}
	}
	fields := strings.Fields(string(stat[closeIdx+1:]))
	if len(fields) < 13 {
		return 0, 0, cpuSample{}
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64) // field 14 overall
	stime, err2 := strconv.ParseUint(fields[12], 10, 64) // field 15 overall
	if err1 != nil || err2 != nil {
		return 0, 0, cpuSample{}
	}
	now := time.Now()
	sample = cpuSample{valid: true, pid: pid, at: now, utime: utime, stime: stime}
	if prev.valid && prev.pid == pid && now.After(prev.at) {
		var delta uint64
		if utime >= prev.utime && stime >= prev.stime {
			delta = (utime - prev.utime) + (stime - prev.stime)
		}
		if delta > 0 {
			seconds := now.Sub(prev.at).Seconds()
			if seconds > 0 {
				cpu = 100 * (float64(delta) / 100.0) / seconds // USER_HZ is 100 on Linux
			}
		}
	}

	statusBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err == nil {
		for _, l := range strings.Split(string(statusBytes), "\n") {
			if !strings.HasPrefix(l, "VmRSS:") {
				continue
			}
			f := strings.Fields(l)
			if len(f) >= 2 {
				if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					memMB = kb / 1024
				}
			}
			break
		}
	}
	return cpu, memMB, sample
}
