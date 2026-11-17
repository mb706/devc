// Retention of mutable file generations (finding E1-04). The archive is
// the user's backup: neither a box (which writes shared harness state)
// nor an enrolled machine (which says what it uploads) may empty it. So
// the hub decides, by its own clock, which generations of a mutable file
// stay:
//
//   - the newest KeepMutable complete generations, and
//   - for every checkpoint, the newest generation completed by then.
//     Checkpoints are the starts of the last keep_daily days and of the
//     last keep_weekly weeks (UTC; weeks start on Monday).
//
// The state a file had at any checkpoint in the window can therefore be
// restored however many generations were uploaded since. Append-only
// generations, complete generations without a recorded time and
// generations newer than the newest complete one are never dropped.
// When the hub's clock is behind a time it recorded for the file (the
// clock stepped back), nothing is dropped until it catches up: a window
// that moved back could otherwise give up a checkpoint still in force.

package hub

import (
	"sort"
	"time"

	"github.com/mb706/devc/internal/archive"
)

// Default retention window ([archive] keep_daily, keep_weekly).
const (
	DefaultKeepDaily  = 7
	DefaultKeepWeekly = 4
)

// genInfo is what retention knows of one generation of a file.
type genInfo struct {
	Gen      int
	Mutable  bool      // recorded as mutable
	Complete bool      // a completion record exists
	Done     time.Time // first completion by the hub's clock; zero: none recorded
}

// checkpoints are the starts of the last keepDaily days (today's
// included) and of the last keepWeekly weeks (this week's included), UTC,
// at now.
func checkpoints(now time.Time, keepDaily, keepWeekly int) []time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var out []time.Time
	for i := 0; i < keepDaily; i++ {
		out = append(out, day.AddDate(0, 0, -i))
	}
	// Monday is weekday 1; Sunday (0) belongs to the week that began six
	// days before.
	week := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	for i := 0; i < keepWeekly; i++ {
		out = append(out, week.AddDate(0, 0, -7*i))
	}
	return out
}

// retentionDrops chooses the generations of one file that may be dropped
// at now. It returns generation numbers, newest first; only generations
// recorded as mutable are ever among them.
func retentionDrops(gens []genInfo, now time.Time, keepDaily, keepWeekly int) []int {
	for _, g := range gens {
		if !g.Done.IsZero() && g.Done.After(now) {
			return nil // the clock is behind the file's own records
		}
	}
	sorted := append([]genInfo(nil), gens...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Gen > sorted[j].Gen })
	keep := map[int]bool{}
	newestComplete := -1
	n := 0
	for _, g := range sorted {
		if !g.Complete {
			continue
		}
		if newestComplete < 0 {
			newestComplete = g.Gen
		}
		if n < archive.KeepMutable {
			keep[g.Gen] = true
			n++
		}
	}
	for _, c := range checkpoints(now, keepDaily, keepWeekly) {
		for _, g := range sorted {
			if g.Complete && !g.Done.IsZero() && !g.Done.After(c) {
				keep[g.Gen] = true
				break
			}
		}
	}
	var drop []int
	for _, g := range sorted {
		if !g.Mutable || keep[g.Gen] {
			continue
		}
		switch {
		case g.Complete && !g.Done.IsZero():
			drop = append(drop, g.Gen)
		case !g.Complete && g.Gen < newestComplete:
			// Never restored: a newer complete generation exists, and an
			// incomplete one is never the state at a time.
			drop = append(drop, g.Gen)
		}
	}
	return drop
}

// completionTime is the time a generation completing now records: the
// hub's clock, but never earlier than the time of an older generation of
// the same file, so the times never decrease with the generation number
// even when the clock steps back.
func completionTime(clock time.Time, older []time.Time) time.Time {
	t := clock.UTC()
	for _, o := range older {
		if o.After(t) {
			t = o
		}
	}
	return t
}
