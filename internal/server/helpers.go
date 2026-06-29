package server

import (
	"strconv"
	"time"
)

// timeValue accepts either a time.Time or a *time.Time and returns the
// dereferenced time + ok flag. Templates pass values by their underlying
// type; this lets the helpers stay tolerant.
func timeValue(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, !t.IsZero()
	case *time.Time:
		if t == nil {
			return time.Time{}, false
		}
		return *t, !t.IsZero()
	}
	return time.Time{}, false
}

func itoa(n int) string     { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
