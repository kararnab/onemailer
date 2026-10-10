package onemailer

import (
	"strconv"
	"time"
)

// Lifetime words a link lifetime for email copy: whole hours when it is a
// whole number of hours ("1 hour", "48 hours"), otherwise whole minutes,
// rounded down ("30 minutes", "90 minutes"), never less than a minute.
func Lifetime(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return plural(int(d/time.Hour), "hour")
	}
	m := int(d / time.Minute)
	if m < 1 {
		m = 1
	}
	return plural(m, "minute")
}

func plural(n int, unit string) string {
	s := strconv.Itoa(n) + " " + unit
	if n != 1 {
		s += "s"
	}
	return s
}
