package hub

import (
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/mb706/devc/internal/archive"
)

// stateAt is the generation that held a file's state at c: the newest
// complete one with a recorded time at or before c (-1: none).
func stateAt(gens []genInfo, c time.Time) int {
	best := -1
	for _, g := range gens {
		if g.Complete && !g.Done.IsZero() && !g.Done.After(c) && g.Gen > best {
			best = g.Gen
		}
	}
	return best
}

// checkDrops verifies the rules every drop set obeys: only mutable
// generations, never one without a recorded time unless it is incomplete
// and older than a complete one, never the newest three complete, never
// the state at a checkpoint of the window at now.
func checkDrops(t *testing.T, gens []genInfo, drops []int, now time.Time, daily, weekly int) {
	t.Helper()
	dropped := map[int]bool{}
	for _, d := range drops {
		dropped[d] = true
	}
	byGen := map[int]genInfo{}
	newestComplete := -1
	var complete []int
	for _, g := range gens {
		byGen[g.Gen] = g
		if g.Complete {
			complete = append(complete, g.Gen)
			newestComplete = max(newestComplete, g.Gen)
		}
	}
	for d := range dropped {
		g, ok := byGen[d]
		switch {
		case !ok:
			t.Errorf("dropped generation %d that does not exist", d)
		case !g.Mutable:
			t.Errorf("dropped generation %d, which is not mutable", d)
		case g.Complete && g.Done.IsZero():
			t.Errorf("dropped complete generation %d without a recorded time", d)
		case !g.Complete && g.Gen > newestComplete:
			t.Errorf("dropped incomplete generation %d, newer than every complete one", d)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(complete)))
	for i := 0; i < len(complete) && i < archive.KeepMutable; i++ {
		if dropped[complete[i]] {
			t.Errorf("dropped generation %d, one of the newest %d complete", complete[i], archive.KeepMutable)
		}
	}
	for _, c := range checkpoints(now, daily, weekly) {
		if s := stateAt(gens, c); s >= 0 && dropped[s] {
			t.Errorf("dropped generation %d, the state at the checkpoint %s", s, c.Format(time.RFC3339))
		}
	}
}

func TestCheckpoints(t *testing.T) {
	// Wednesday 2026-11-18 15:04 UTC.
	now := time.Date(2026, 11, 18, 15, 4, 0, 0, time.UTC)
	got := checkpoints(now, 3, 2)
	want := []string{"2026-11-18", "2026-11-17", "2026-11-16", "2026-11-16", "2026-11-09"}
	if len(got) != len(want) {
		t.Fatalf("checkpoints %v", got)
	}
	for i, c := range got {
		if c.Format("2006-01-02") != want[i] || c.Hour() != 0 || c.Location() != time.UTC {
			t.Errorf("checkpoint %d: %s, want %s", i, c, want[i])
		}
	}
	// A Sunday belongs to the week that began on the Monday before; a time
	// zone of the caller does not matter.
	sun := time.Date(2026, 11, 22, 23, 0, 0, 0, time.FixedZone("x", -5*3600)) // Monday 04:00 UTC
	if w := checkpoints(sun, 0, 1); w[0].Format("2006-01-02") != "2026-11-23" {
		t.Errorf("week of %s: %s", sun, w[0])
	}
	if w := checkpoints(time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC), 0, 1); w[0].Format("2006-01-02") != "2026-11-16" {
		t.Errorf("week of a Sunday: %s", w[0])
	}
	if len(checkpoints(now, 0, 0)) != 0 {
		t.Error("checkpoints without a window")
	}
}

func TestRetentionDrops(t *testing.T) {
	now := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	today := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	mut := func(g int, done time.Time) genInfo { return genInfo{Gen: g, Mutable: true, Complete: true, Done: done} }
	set := func(ds []int) map[int]bool {
		m := map[int]bool{}
		for _, d := range ds {
			m[d] = true
		}
		return m
	}

	t.Run("unchanged for months, then rewritten many times in one day", func(t *testing.T) {
		gens := []genInfo{mut(0, now.AddDate(0, -4, 0))}
		for g := 1; g <= 40; g++ {
			gens = append(gens, mut(g, today.Add(8*time.Hour+time.Duration(g)*time.Minute)))
		}
		drops := retentionDrops(gens, now, 7, 4)
		checkDrops(t, gens, drops, now, 7, 4)
		d := set(drops)
		if d[0] {
			t.Error("the state of the last months was dropped")
		}
		for g := 1; g <= 37; g++ {
			if !d[g] {
				t.Errorf("generation %d of today was kept", g)
			}
		}
		if len(drops) != 37 {
			t.Errorf("%d dropped, want 37", len(drops))
		}
	})

	t.Run("many generations within one day", func(t *testing.T) {
		yesterday := today.AddDate(0, 0, -1)
		var gens []genInfo
		for g := 0; g < 50; g++ {
			gens = append(gens, mut(g, yesterday.Add(time.Hour+time.Duration(g)*10*time.Minute)))
		}
		drops := retentionDrops(gens, now, 7, 4)
		checkDrops(t, gens, drops, now, 7, 4)
		if len(drops) != 47 || set(drops)[49] {
			t.Errorf("drops %v", drops)
		}
		// One more today: yesterday's last state stays as the state at
		// today's start.
		gens = append(gens, mut(50, today.Add(time.Hour)), mut(51, today.Add(2*time.Hour)), mut(52, today.Add(3*time.Hour)))
		drops = retentionDrops(gens, now, 7, 4)
		checkDrops(t, gens, drops, now, 7, 4)
		if set(drops)[49] {
			t.Error("the state at today's start was dropped")
		}
	})

	t.Run("a clock that steps backwards", func(t *testing.T) {
		gens := []genInfo{mut(0, now.Add(-72*time.Hour)), mut(1, now.Add(-48*time.Hour)), mut(2, now.Add(-time.Hour)),
			mut(3, now.Add(-time.Minute)), mut(4, now.Add(time.Hour)), mut(5, now.Add(2*time.Hour))}
		if drops := retentionDrops(gens, now, 0, 0); len(drops) != 0 {
			t.Errorf("dropped %v while the clock is behind the records", drops)
		}
		// Once it caught up, the usual rules apply.
		later := now.Add(3 * time.Hour)
		drops := retentionDrops(gens, later, 0, 0)
		checkDrops(t, gens, drops, later, 0, 0)
		if len(drops) != 3 {
			t.Errorf("after the clock caught up: %v", drops)
		}
		// The times recorded never decrease, whatever the clock does.
		clock := []time.Time{now, now.Add(-time.Hour), now.Add(-48 * time.Hour), now.Add(time.Minute), now}
		var rec []time.Time
		for _, c := range clock {
			ct := completionTime(c, rec)
			if len(rec) > 0 && ct.Before(rec[len(rec)-1]) {
				t.Errorf("a recorded time went back: %s after %s", ct, rec[len(rec)-1])
			}
			if ct.Before(c) {
				t.Errorf("a recorded time before the clock: %s, clock %s", ct, c)
			}
			rec = append(rec, ct)
		}
	})

	t.Run("generations without a recorded time", func(t *testing.T) {
		gens := []genInfo{
			{Gen: 0, Mutable: true, Complete: true}, // complete before times were recorded
			{Gen: 1, Mutable: true},                 // incomplete, older than a complete one
			mut(2, now.Add(-30*24*time.Hour)), mut(3, now.Add(-29*24*time.Hour)),
			mut(4, now.Add(-3*time.Hour)), mut(5, now.Add(-2*time.Hour)), mut(6, now.Add(-time.Hour)),
			{Gen: 7, Mutable: true}, // incomplete, newer than every complete one
		}
		drops := retentionDrops(gens, now, 7, 4)
		checkDrops(t, gens, drops, now, 7, 4)
		// Generation 3 is the state at every checkpoint of the window;
		// 4 to 6 are the newest three.
		if d := set(drops); len(drops) != 2 || !d[1] || !d[2] {
			t.Errorf("drops %v, want 1 and 2", drops)
		}
	})

	t.Run("append-only generations are never dropped", func(t *testing.T) {
		var gens []genInfo
		for g := 0; g < 30; g++ {
			gens = append(gens, genInfo{Gen: g, Complete: true, Done: now.Add(-time.Duration(30-g) * time.Hour)}, genInfo{Gen: 100 + g})
		}
		if drops := retentionDrops(gens, now, 7, 4); len(drops) != 0 {
			t.Errorf("dropped append-only generations %v", drops)
		}
	})
}

// TestRetentionHostile plays a machine that completes generations of one
// file at random over months, with incomplete ones in between and a hub
// clock that sometimes steps back. Retention runs at every completion, as
// the hub runs it, and its drops are applied. After every run, the state
// at every checkpoint of the window at the newest time the clock showed
// is still there, and what remains stays bounded.
func TestRetentionHostile(t *testing.T) {
	const daily, weekly = 7, 4
	bound := archive.KeepMutable + daily + weekly
	for seed := uint64(1); seed <= 40; seed++ {
		r := rand.New(rand.NewPCG(seed, 99))
		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		clock, trueNow := start, start
		var all, kept []genInfo // the whole history; what the hub still holds
		gen := 0
		for step := 0; step < 600; step++ {
			// Time moves on by up to two days; now and then the clock steps
			// back by up to a day.
			adv := time.Duration(r.Int64N(int64(48 * time.Hour)))
			if r.IntN(4) == 0 {
				adv = time.Duration(r.Int64N(int64(10 * time.Minute)))
			}
			clock = clock.Add(adv)
			if r.IntN(20) == 0 {
				clock = clock.Add(-time.Duration(r.Int64N(int64(24 * time.Hour))))
			}
			if clock.After(trueNow) {
				trueNow = clock
			}
			if r.IntN(5) == 0 {
				g := genInfo{Gen: gen, Mutable: true}
				all, kept = append(all, g), append(kept, g)
				gen++
				continue
			}
			var older []time.Time
			for _, k := range kept {
				if !k.Done.IsZero() {
					older = append(older, k.Done)
				}
			}
			g := genInfo{Gen: gen, Mutable: true, Complete: true, Done: completionTime(clock, older)}
			gen++
			all, kept = append(all, g), append(kept, g)
			drops := retentionDrops(kept, clock, daily, weekly)
			checkDrops(t, kept, drops, clock, daily, weekly)
			gone := map[int]bool{}
			for _, d := range drops {
				gone[d] = true
			}
			var next []genInfo
			for _, k := range kept {
				if !gone[k.Gen] {
					next = append(next, k)
				}
			}
			kept = next
			have := map[int]bool{}
			timed := 0
			for _, k := range kept {
				have[k.Gen] = true
				if k.Complete {
					timed++
				}
			}
			for _, c := range checkpoints(trueNow, daily, weekly) {
				if s := stateAt(all, c); s >= 0 && !have[s] {
					t.Fatalf("seed %d step %d: the state at %s (generation %d) is gone", seed, step, c.Format(time.RFC3339), s)
				}
			}
			behind := false
			for _, k := range kept {
				behind = behind || k.Done.After(clock)
			}
			if !behind && timed > bound {
				t.Fatalf("seed %d step %d: %d complete generations remain, more than %d", seed, step, timed, bound)
			}
		}
	}
}
