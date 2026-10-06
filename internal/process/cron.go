package process

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Cron schedules use the five-field syntax Kubernetes CronJobs accept
// (minute hour day-of-month month day-of-week, in UTC) or one of the
// macros. Checking them here makes a typo fail the build with a clear
// message instead of a CronJob the API server rejects at deploy time.

var macros = map[string]bool{
	"@yearly": true, "@annually": true, "@monthly": true, "@weekly": true,
	"@daily": true, "@midnight": true, "@hourly": true,
}

type cronField struct {
	name     string
	min, max int
	names    []string // names[i] stands for min+i
}

var cronFields = []cronField{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12,
		names: []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}},
	{name: "day of week", min: 0, max: 6, names: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}},
}

// ValidSchedule checks a cron schedule: five fields of *, values, ranges
// (a-b), steps (*/n, a-b/n) and lists (a,b), month and weekday names, or
// @hourly, @daily, @weekly, @monthly, @yearly (@annually, @midnight).
func ValidSchedule(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("schedule is empty")
	}
	if strings.HasPrefix(s, "@") {
		if macros[strings.ToLower(s)] {
			return nil
		}
		return fmt.Errorf("schedule %q: unknown macro (use @hourly, @daily, @weekly, @monthly or @yearly)", s)
	}
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return fmt.Errorf("schedule %q must have 5 fields (minute hour day-of-month month day-of-week), got %d",
			s, len(fields))
	}
	for i, f := range fields {
		if err := cronFields[i].check(f); err != nil {
			return fmt.Errorf("schedule %q: %w", s, err)
		}
	}
	return nil
}

func (cf cronField) check(field string) error {
	for _, item := range strings.Split(field, ",") {
		if err := cf.checkItem(item); err != nil {
			return err
		}
	}
	return nil
}

func (cf cronField) checkItem(item string) error {
	bad := func(why string) error { return fmt.Errorf("%s field %q: %s", cf.name, item, why) }
	rng, step, hasStep := strings.Cut(item, "/")
	if hasStep {
		n, err := strconv.Atoi(step)
		if err != nil || n < 1 || n > cf.max-cf.min+1 {
			return bad(fmt.Sprintf("step must be a number between 1 and %d", cf.max-cf.min+1))
		}
	}
	if rng == "*" || (rng == "?" && (cf.name == "day of month" || cf.name == "day of week")) {
		return nil
	}
	lo, hi, isRange := strings.Cut(rng, "-")
	a, err := cf.value(lo)
	if err != nil {
		return bad(err.Error())
	}
	if !isRange {
		return nil
	}
	b, err := cf.value(hi)
	if err != nil {
		return bad(err.Error())
	}
	if a > b {
		return bad("range start is after its end")
	}
	return nil
}

func (cf cronField) value(v string) (int, error) {
	for i, n := range cf.names {
		if strings.EqualFold(v, n) {
			return cf.min + i, nil
		}
	}
	n, err := strconv.Atoi(v)
	if err != nil || v == "" || v[0] == '+' || v[0] == '-' {
		return 0, fmt.Errorf("%q is not a value", v)
	}
	if n < cf.min || n > cf.max {
		return 0, fmt.Errorf("%d is outside %d-%d", n, cf.min, cf.max)
	}
	return n, nil
}
