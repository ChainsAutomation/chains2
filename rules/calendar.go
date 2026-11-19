package rules

import (
	"errors"
	"fmt"
	"time"
)

// WeekConvention describes a locale's first weekday and minimum days in week one.
type WeekConvention struct {
	FirstDay time.Weekday
	MinDays  int
}

type Calendar struct {
	Zone       *time.Location
	Week       string
	Disabled   bool
	LocaleWeek *WeekConvention
}

func (c Calendar) next(name string, args []any, now time.Time) (time.Time, error) {
	if name == "Every" {
		if len(args) == 1 {
			if interval, ok := args[0].(string); ok {
				d, err := time.ParseDuration(interval)
				if err == nil && d >= time.Millisecond {
					return now.Add(d), nil
				}
			}
		}
		return time.Time{}, errors.New("Every expects a duration of at least 1ms")
	}
	fields, err := pairs(args)
	if err != nil {
		return time.Time{}, err
	}
	if c.Zone == nil {
		c.Zone = time.Local
	}
	bounds := map[string][2]int{"year": {1, 9999}, "month": {1, 12}, "week": {0, 53}, "day": {1, 31}, "weekday": {0, 6}, "hour": {0, 23}, "minute": {0, 59}, "second": {0, 59}}
	spec := map[string]int{}
	for key, value := range fields {
		n, ok := number(value)
		b, known := bounds[key]
		if !ok || !known || n < float64(b[0]) || n > float64(b[1]) || n != float64(int(n)) {
			return time.Time{}, fmt.Errorf("invalid calendar field %q", key)
		}
		spec[key] = int(n)
	}
	if _, ok := spec["second"]; !ok {
		spec["second"] = 0
	}
	if _, h := fields["hour"]; !h {
		if _, m := fields["minute"]; !m {
			if _, s := fields["second"]; !s {
				spec["hour"] = 0
			}
		}
	}
	if _, m := fields["minute"]; !m {
		if _, s := fields["second"]; !s {
			if _, h := spec["hour"]; h {
				spec["minute"] = 0
			}
		}
	}
	local := now.In(c.Zone)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	limit := day.AddDate(40, 0, 0)
	if y, ok := spec["year"]; ok {
		if y < local.Year() {
			return time.Time{}, errors.New("calendar year has elapsed")
		}
		if y > local.Year() {
			day = time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		limit = time.Date(y+1, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	for ; day.Before(limit); day = day.AddDate(0, 0, 1) {
		_, week := day.ISOWeek()
		if c.Week == "sunday" {
			week = (day.YearDay() + 6 - int(day.Weekday())) / 7
		} else if spec := c.LocaleWeek; spec != nil {
			week = spec.week(day)
		}
		if !check(spec, "year", day.Year()) || !check(spec, "month", int(day.Month())) || !check(spec, "day", day.Day()) || !check(spec, "weekday", int(day.Weekday())) || !check(spec, "week", week) {
			continue
		}
		for h := 0; h < 24; h++ {
			if !check(spec, "hour", h) {
				continue
			}
			for m := 0; m < 60; m++ {
				if !check(spec, "minute", m) {
					continue
				}
				wall := day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(spec["second"])*time.Second)
				candidate := firstWallTime(wall, c.Zone)
				if candidate.After(now) {
					return candidate, nil
				}
			}
		}
	}
	return time.Time{}, errors.New("calendar selector has no future match in the search window")
}

func check(fields map[string]int, key string, v int) bool {
	expected, ok := fields[key]
	return !ok || expected == v
}

func firstWallTime(wall time.Time, zone *time.Location) time.Time {
	var first time.Time
	for _, delta := range []time.Duration{-36 * time.Hour, 0, 36 * time.Hour} {
		_, offset := wall.Add(delta).In(zone).Zone()
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(zone)
		if local.Year() == wall.Year() && local.YearDay() == wall.YearDay() && local.Hour() == wall.Hour() && local.Minute() == wall.Minute() && local.Second() == wall.Second() && (first.IsZero() || candidate.Before(first)) {
			first = candidate
		}
	}
	return first
}

func (c WeekConvention) week(day time.Time) int {
	first := c.weekOne(day.Year())
	switch {
	case day.Before(first):
		first = c.weekOne(day.Year() - 1)
	case !day.Before(c.weekOne(day.Year() + 1)):
		return 1
	}
	return int(day.Sub(first)/(7*24*time.Hour)) + 1
}

func (c WeekConvention) weekOne(year int) time.Time {
	jan := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
	prior := (int(jan.Weekday()) - int(c.FirstDay) + 7) % 7
	if 7-prior < c.MinDays {
		return jan.AddDate(0, 0, 7-prior)
	}
	return jan.AddDate(0, 0, -prior)
}
