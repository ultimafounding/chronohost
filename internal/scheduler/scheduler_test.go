package scheduler

import (
	"testing"
	"time"
)

func TestParseAndNext(t *testing.T) {
	from := time.Date(2026, 11, 26, 2, 30, 0, 0, time.UTC)

	spec, err := Parse("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	next := spec.Next(from)
	if next.Format(time.RFC3339) != "2026-11-26T03:00:00Z" {
		t.Fatalf("daily next = %s", next)
	}

	spec, err = Parse("@every 90m")
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Next(from); !got.Equal(from.Add(90 * time.Minute)) {
		t.Fatalf("@every next = %s", got)
	}

	spec, err = Parse("*/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Next(from); !got.Equal(time.Date(2026, 11, 26, 2, 45, 0, 0, time.UTC)) {
		t.Fatalf("step next = %s", got)
	}

	spec, err = Parse("@weekly")
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Next(from).Weekday(); got != time.Sunday {
		t.Fatalf("weekly next weekday = %s", got)
	}

	for _, expr := range []string{"", "not-a-cron", "60 * * * *", "0 3 * *"} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) unexpectedly succeeded", expr)
		}
	}
}

func TestDefaultSchedule(t *testing.T) {
	jobs := Defaults()
	if len(jobs) != 1 || jobs[0].Kind != "backup" || !jobs[0].Enabled {
		t.Fatalf("unexpected defaults: %+v", jobs)
	}
	if _, err := Parse(jobs[0].Expr); err != nil {
		t.Fatalf("default expression invalid: %v", err)
	}
}
