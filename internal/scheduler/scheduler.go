// Package scheduler runs recurring backup, restart and announcement jobs and
// broadcasts warnings into the game console before disruptive actions.
package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackm/chronohost/internal/backup"
	"github.com/jackm/chronohost/internal/notify"
	"github.com/jackm/chronohost/internal/process"
	"github.com/jackm/chronohost/internal/store"
)

// SpecBuilder resolves the process spec for an instance at job time. It is
// supplied by the API layer so the scheduler never needs to know about the
// binary cache layout.
type SpecBuilder func(store.Instance) (process.Spec, error)

// Scheduler periodically evaluates persisted schedules.
type Scheduler struct {
	store    *store.Store
	procs    *process.Manager
	backups  *backup.Engine
	notifier *notify.Notifier
	spec     SpecBuilder
	logf     func(string, ...any)

	mu      sync.Mutex
	pending map[string]bool
}

// New constructs a scheduler.
func New(st *store.Store, procs *process.Manager, beng *backup.Engine, n *notify.Notifier, spec SpecBuilder, logf func(string, ...any)) *Scheduler {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Scheduler{store: st, procs: procs, backups: beng, notifier: n, spec: spec, logf: logf, pending: map[string]bool{}}
}

// Start runs the scheduler until ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		if err := s.RunDue(ctx); err != nil {
			s.logf("scheduler: initial run: %v", err)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.RunDue(ctx); err != nil {
					s.logf("scheduler: %v", err)
				}
			}
		}
	}()
}

// RunDue executes every schedule whose next run time has passed.
func (s *Scheduler) RunDue(ctx context.Context) error {
	jobs, err := s.store.ListAllSchedules(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, job := range jobs {
		spec, err := Parse(job.Expr)
		if err != nil {
			s.logf("scheduler: job %d has invalid expression %q: %v", job.ID, job.Expr, err)
			continue
		}
		base := job.CreatedAt
		if job.LastRun != nil && job.LastRun.After(base) {
			base = *job.LastRun
		}
		if base.IsZero() {
			base = now.Add(-time.Hour)
		}
		next := spec.Next(base)
		if next.After(now) {
			continue
		}
		// Guard against executing the same occurrence twice from overlapping ticks.
		key := fmt.Sprintf("%d-%s", job.ID, next.Format(time.RFC3339))
		s.mu.Lock()
		if s.pending[key] {
			s.mu.Unlock()
			continue
		}
		s.pending[key] = true
		s.mu.Unlock()

		if err := s.store.MarkScheduleRun(ctx, job.ID, now); err != nil {
			s.logf("scheduler: mark job %d: %v", job.ID, err)
		}
		s.execute(ctx, job)
	}
	return nil
}

func (s *Scheduler) execute(ctx context.Context, job store.Schedule) {
	in, err := s.store.GetInstance(ctx, job.InstanceID)
	if err != nil {
		s.logf("scheduler: instance for job %d: %v", job.ID, err)
		return
	}
	switch job.Kind {
	case "backup":
		snap, err := s.backups.Create(job.InstanceID, in.DataPath, "scheduled")
		if err != nil {
			s.logf("scheduler: backup for %s: %v", in.Name, err)
			return
		}
		record, err := s.store.AddBackup(ctx, store.Backup{
			InstanceID: in.ID, Name: snap.Name, Path: snap.Path, Size: snap.Size,
			Kind: snap.Kind, CreatedAt: snap.CreatedAt, Files: snap.Files,
		})
		if err != nil {
			s.logf("scheduler: record backup for %s: %v", in.Name, err)
		}
		s.applyRetention(ctx, in.ID)
		name := snap.Name
		if err == nil {
			name = record.Name
		}
		s.notifier.Send(ctx, notify.Event{Instance: in.Name, Type: "backup", Detail: fmt.Sprintf("Scheduled snapshot %s created", name), Time: time.Now()})
	case "announce":
		msg := job.Message
		if msg == "" {
			msg = "ChronoHost: temporal stability is nominal."
		}
		if err := s.procs.Command(job.InstanceID, "/say "+msg); err != nil {
			s.logf("scheduler: announce on %s: %v", in.Name, err)
		}
	case "restart":
		// Broadcast a warning first, then perform the restart after a grace period.
		if err := s.procs.Command(job.InstanceID, "/say [ChronoHost] Scheduled restart in 60 seconds - please log out."); err != nil {
			s.logf("scheduler: warning on %s: %v", in.Name, err)
		}
		timer := time.NewTimer(60 * time.Second)
		go func() {
			defer timer.Stop()
			<-timer.C
			if ctx.Err() != nil {
				return
			}
			spec, err := s.spec(in)
			if err != nil {
				s.logf("scheduler: restart spec for %s: %v", in.Name, err)
				return
			}
			if err := s.procs.Restart(spec); err != nil {
				s.logf("scheduler: restart %s: %v", in.Name, err)
			}
		}()
	default:
		s.logf("scheduler: unknown job kind %q", job.Kind)
	}
}

// applyRetention keeps the on-disk snapshots and their database records in
// sync after a scheduled backup.
func (s *Scheduler) applyRetention(ctx context.Context, instanceID string) {
	policy := backup.DefaultPolicy()
	settings := map[string]*int{
		"backup_hourly": &policy.Hourly, "backup_daily": &policy.Daily,
		"backup_weekly": &policy.Weekly, "backup_monthly": &policy.Monthly,
	}
	for key, target := range settings {
		if n, err := strconv.Atoi(s.store.GetSetting(ctx, key, "")); err == nil && n >= 0 {
			*target = n
		}
	}
	kept, err := s.backups.ApplyRetention(instanceID, policy)
	if err != nil {
		s.logf("scheduler: retention for %s: %v", instanceID, err)
		return
	}
	keepPaths := map[string]bool{}
	for _, snap := range kept {
		keepPaths[snap.Path] = true
	}
	records, err := s.store.ListBackups(ctx, instanceID)
	if err != nil {
		return
	}
	for _, record := range records {
		if keepPaths[record.Path] {
			continue
		}
		snap := backup.Snapshot{
			ID: fmt.Sprintf("%d", record.ID), InstanceID: record.InstanceID,
			Name: record.Name, Path: record.Path, Size: record.Size,
			Kind: record.Kind, CreatedAt: record.CreatedAt, Files: record.Files,
		}
		if err := s.backups.Delete(snap); err == nil {
			_ = s.store.DeleteBackup(ctx, record.ID)
		}
	}
}

// Spec is a parsed schedule expression.
type Spec struct {
	every    time.Duration
	minutes  intSet // minute of hour
	hours    intSet // hour of day
	days     intSet // day of month
	months   intSet // month
	weekdays intSet // day of week, 0=Sunday
}

// Parse accepts @every <duration>, @hourly, @daily, @weekly, @monthly or a
// standard five-field crontab expression (minute hour dom month dow).
func Parse(expr string) (Spec, error) {
	expr = strings.TrimSpace(expr)
	var s Spec
	switch {
	case strings.HasPrefix(expr, "@every "):
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(expr, "@every ")))
		if err != nil || d <= 0 {
			return s, fmt.Errorf("invalid @every duration: %q", expr)
		}
		s.every = d
		return s, nil
	case expr == "@hourly":
		s.minutes = allSet(0, 59)
		s.hours, s.days, s.months, s.weekdays = allSet(0, 23), allSet(1, 31), allSet(1, 12), allSet(0, 6)
	case expr == "@daily":
		s.minutes, s.hours, s.days, s.months, s.weekdays = setOf(0), setOf(0), allSet(1, 31), allSet(1, 12), allSet(0, 6)
	case expr == "@weekly":
		s.minutes, s.hours, s.days, s.months, s.weekdays = setOf(0), setOf(0), allSet(1, 31), allSet(1, 12), setOf(0)
	case expr == "@monthly":
		s.minutes, s.hours, s.days, s.months, s.weekdays = setOf(0), setOf(0), setOf(1), allSet(1, 12), allSet(0, 6)
	default:
		fields := strings.Fields(expr)
		if len(fields) != 5 {
			return s, fmt.Errorf("expected 5 cron fields or @expression, got %q", expr)
		}
		var err error
		if s.minutes, err = parseField(fields[0], 0, 59); err != nil {
			return s, fmt.Errorf("minute: %w", err)
		}
		if s.hours, err = parseField(fields[1], 0, 23); err != nil {
			return s, fmt.Errorf("hour: %w", err)
		}
		if s.days, err = parseField(fields[2], 1, 31); err != nil {
			return s, fmt.Errorf("day: %w", err)
		}
		if s.months, err = parseField(fields[3], 1, 12); err != nil {
			return s, fmt.Errorf("month: %w", err)
		}
		if s.weekdays, err = parseField(fields[4], 0, 6); err != nil {
			return s, fmt.Errorf("weekday: %w", err)
		}
	}
	return s, nil
}

// Next returns the first time at or after from that matches the expression.
func (s Spec) Next(from time.Time) time.Time {
	if s.every > 0 {
		return from.Add(s.every)
	}
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(1, 0, 0)
	for ; t.Before(limit); t = t.Add(time.Minute) {
		if !s.minutes[t.Minute()] || !s.hours[t.Hour()] ||
			!s.months[int(t.Month())] || !s.days[t.Day()] ||
			!s.weekdays[int(t.Weekday())] {
			continue
		}
		return t
	}
	return time.Time{}
}

type intSet map[int]bool

func setOf(values ...int) intSet {
	s := intSet{}
	for _, v := range values {
		s[v] = true
	}
	return s
}

func allSet(min, max int) intSet {
	s := intSet{}
	for i := min; i <= max; i++ {
		s[i] = true
	}
	return s
}

func parseField(field string, min, max int) (intSet, error) {
	out := intSet{}
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty value")
		}
		step := 1
		if idx := strings.Index(part, "/"); idx >= 0 {
			var err error
			step, err = strconv.Atoi(part[idx+1:])
			if err != nil || step < 1 {
				return nil, fmt.Errorf("invalid step %q", part)
			}
			part = part[:idx]
		}
		if part == "*" {
			for i := min; i <= max; i += step {
				out[i] = true
			}
			continue
		}
		lo, hi := min, max
		if idx := strings.Index(part, "-"); idx >= 0 {
			var err error
			lo, err = strconv.Atoi(part[:idx])
			if err != nil {
				return nil, fmt.Errorf("invalid range %q", part)
			}
			hi, err = strconv.Atoi(part[idx+1:])
			if err != nil {
				return nil, fmt.Errorf("invalid range %q", part)
			}
		} else {
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid value %q", part)
			}
			lo, hi = n, n
		}
		if lo < min || hi > max || lo > hi {
			return nil, fmt.Errorf("%d-%d outside %d-%d", lo, hi, min, max)
		}
		for i := lo; i <= hi; i += step {
			out[i] = true
		}
	}
	return out, nil
}

// Defaults returns the default backup schedule expressions used on creation.
func Defaults() []store.Schedule {
	now := time.Now()
	return []store.Schedule{
		{Kind: "backup", Expr: "0 3 * * *", Enabled: true, CreatedAt: now},
	}
}
