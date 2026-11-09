package crawler

import (
	"testing"
	"time"
)

func TestNextAfter(t *testing.T) {
	loc := time.FixedZone("EAT", 3*3600)
	s := &Scheduler{Hour: 3, Minute: 0, Loc: loc}
	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 11, 9, 2, 59, 0, 0, loc), time.Date(2026, 11, 9, 3, 0, 0, 0, loc)},
		{time.Date(2026, 11, 9, 3, 0, 0, 0, loc), time.Date(2026, 11, 10, 3, 0, 0, 0, loc)},
		{time.Date(2026, 12, 31, 12, 0, 0, 0, loc), time.Date(2027, 1, 1, 3, 0, 0, 0, loc)},
		{time.Date(2026, 11, 9, 0, 30, 0, 0, time.UTC), time.Date(2026, 11, 10, 3, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		if got := s.NextAfter(c.now); !got.Equal(c.want) {
			t.Errorf("NextAfter(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}
