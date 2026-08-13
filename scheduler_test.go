package main

import (
	"testing"
	"time"
)

func TestNextRunTime(t *testing.T) {
	// The user's config: 06:00 and 12:00 daily (18 exclusive).
	sc := ScheduleConfig{StartHour: 6, EndHour: 18, IntervalHours: 6}
	day := func(d, h, m int) time.Time {
		return time.Date(2026, 6, d, h, m, 0, 0, time.Local)
	}
	cases := []struct {
		name string
		sc   ScheduleConfig
		from time.Time
		want time.Time
	}{
		{"before window", sc, day(12, 5, 0), day(12, 6, 0)},
		{"exactly on slot fires next slot", sc, day(12, 6, 0), day(12, 12, 0)},
		{"mid-morning", sc, day(12, 9, 30), day(12, 12, 0)},
		{"just before noon", sc, day(12, 11, 59), day(12, 12, 0)},
		{"afternoon rolls to next day", sc, day(12, 13, 0), day(13, 6, 0)},
		{"end hour is exclusive", sc, day(12, 12, 1), day(13, 6, 0)}, // no 18:00 slot
		{"late night", sc, day(12, 23, 0), day(13, 6, 0)},
		{
			"4h interval hits 6/10/14",
			ScheduleConfig{StartHour: 6, EndHour: 18, IntervalHours: 4},
			day(12, 11, 0), day(12, 14, 0),
		},
		{
			"single daily 4pm slot",
			ScheduleConfig{StartHour: 16, EndHour: 17, IntervalHours: 24},
			day(12, 16, 1), day(13, 16, 0),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NextRunTime(c.from, c.sc)
			if !got.Equal(c.want) {
				t.Errorf("NextRunTime(%v) = %v, want %v", c.from, got, c.want)
			}
		})
	}
}

func TestSchedulerEnabled(t *testing.T) {
	if !schedulerEnabled(ScheduleConfig{StartHour: 6, EndHour: 18, IntervalHours: 6}) {
		t.Error("valid window reported disabled")
	}
	for _, sc := range []ScheduleConfig{
		{StartHour: 6, EndHour: 18, IntervalHours: 0}, // no interval
		{StartHour: 18, EndHour: 6, IntervalHours: 4}, // inverted window
		{StartHour: 6, EndHour: 6, IntervalHours: 4},  // empty window
	} {
		if schedulerEnabled(sc) {
			t.Errorf("config %+v should be disabled", sc)
		}
	}
}
