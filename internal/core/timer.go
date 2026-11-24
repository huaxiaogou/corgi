package core

import (
	"fmt"
	"strings"
	"time"

	"github.com/kiperjing/corgi/internal/model"
)

var weekdayMap = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}

// ParseTimerSpec computes the initial run time for specs:
// "in 30m", "every 2h", "daily 09:00", "weekdays 09:00", "weekly mon,thu 09:00", "at 2026-11-25 09:00".
func ParseTimerSpec(spec string, now time.Time) (time.Time, error) {
	f := strings.Fields(strings.TrimSpace(spec))
	if len(f) < 2 {
		return time.Time{}, fmt.Errorf("spec 格式应为 'in 30m' | 'every 2h' | 'daily 09:00' | 'weekdays 09:00' | 'weekly mon,thu 09:00' | 'at 2026-11-25 09:00'")
	}
	switch strings.ToLower(f[0]) {
	case "in", "every":
		d, err := time.ParseDuration(f[1])
		if err != nil || d < time.Minute {
			return time.Time{}, fmt.Errorf("时间间隔需 ≥ 1m，例如 30m 或 2h")
		}
		return now.Add(d), nil
	case "daily", "weekdays", "weekly":
		days, clock, err := parseClockSpec(f)
		if err != nil {
			return time.Time{}, err
		}
		next := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
		for !next.After(now) || !days[next.Weekday()] {
			next = next.AddDate(0, 0, 1)
		}
		return next, nil
	case "at":
		t, err := time.ParseInLocation("2006-01-02 15:04", strings.Join(f[1:], " "), now.Location())
		if err != nil {
			return time.Time{}, fmt.Errorf("at 格式需为 'YYYY-MM-DD HH:MM'")
		}
		return t, nil
	default:
		return time.Time{}, fmt.Errorf("未知定时格式 %q", f[0])
	}
}

func parseClockSpec(f []string) (map[time.Weekday]bool, time.Time, error) {
	days := map[time.Weekday]bool{}
	clockIdx := 1
	switch strings.ToLower(f[0]) {
	case "daily":
		for d := time.Sunday; d <= time.Saturday; d++ {
			days[d] = true
		}
	case "weekdays":
		for d := time.Monday; d <= time.Friday; d++ {
			days[d] = true
		}
	case "weekly":
		if len(f) < 3 {
			return nil, time.Time{}, fmt.Errorf("weekly 需要星期与时间，例如 'weekly mon,thu 09:00'")
		}
		for _, tok := range strings.Split(strings.ToLower(f[1]), ",") {
			d, ok := weekdayMap[strings.TrimSpace(tok)]
			if !ok {
				return nil, time.Time{}, fmt.Errorf("未知星期 %q (支持 mon..sun)", tok)
			}
			days[d] = true
		}
		clockIdx = 2
	}
	t, err := time.Parse("15:04", f[clockIdx])
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%s 时间需为 HH:MM", f[0])
	}
	return days, t, nil
}

// AdvanceTimer moves a fired timer to its next occurrence or disables one-shot timers.
func AdvanceTimer(t *model.Timer, now time.Time) {
	f := strings.Fields(strings.TrimSpace(t.Spec))
	if len(f) < 2 {
		t.Enabled = false
		return
	}
	switch strings.ToLower(f[0]) {
	case "every":
		d, err := time.ParseDuration(f[1])
		if err != nil || d < time.Minute {
			t.Enabled = false
			return
		}
		next := t.NextRun
		for !next.After(now) {
			next = next.Add(d)
		}
		t.NextRun = next
	case "daily", "weekdays", "weekly":
		days, _, err := parseClockSpec(f)
		if err != nil {
			t.Enabled = false
			return
		}
		next := t.NextRun
		for !next.After(now) || !days[next.Weekday()] {
			next = next.AddDate(0, 0, 1)
		}
		t.NextRun = next
	default:
		t.Enabled = false
	}
}
