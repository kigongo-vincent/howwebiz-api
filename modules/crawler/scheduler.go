package crawler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/base"
)

// Scheduler fires the default crawl once a day at CRON_AT (HH:MM, in
// CRON_TZ). Env:
//
//	CRON_ENABLED=false   turn the daily crawl off
//	CRON_AT=03:00        wall-clock time of the daily crawl
//	CRON_TZ=Africa/Kampala
//	CRAWL_ON_START=true  also crawl once at boot
//	CATALOG_DAILY=false  skip the catalog step after the daily list crawl
type Scheduler struct {
	Engine       *Engine
	Hooks        []func() // other sources' daily jobs (run in their own goroutine)
	Hour, Minute int
	Loc          *time.Location

	mu   sync.Mutex
	next time.Time
}

// StartScheduler launches the daily crawl loop if enabled and returns it
// (nil when disabled). The loop stops when ctx is cancelled.
func StartScheduler(ctx context.Context, e *Engine, hooks ...func()) *Scheduler {
	if base.EnvBool("CRAWL_ON_START", false) {
		if _, err := e.Start("startup", e.Cfg.Defaults); err != nil {
			slog.Warn("startup crawl not started", "err", err)
		}
	}
	if !base.EnvBool("CRON_ENABLED", true) {
		slog.Info("crawler: daily schedule disabled")
		return nil
	}
	at, err := time.Parse("15:04", base.Env("CRON_AT", "03:00"))
	if err != nil {
		slog.Error("crawler: invalid CRON_AT, want HH:MM; daily schedule disabled", "value", base.Env("CRON_AT", ""))
		return nil
	}
	s := &Scheduler{Engine: e, Hooks: hooks, Hour: at.Hour(), Minute: at.Minute(), Loc: base.Location()}
	go s.loop(ctx)
	return s
}

// NextAfter returns the first scheduled time strictly after t.
func (s *Scheduler) NextAfter(t time.Time) time.Time {
	t = t.In(s.Loc)
	next := time.Date(t.Year(), t.Month(), t.Day(), s.Hour, s.Minute, 0, 0, s.Loc)
	if !next.After(t) {
		next = time.Date(t.Year(), t.Month(), t.Day()+1, s.Hour, s.Minute, 0, 0, s.Loc)
	}
	return next
}

// Next is the upcoming fire time (zero if the scheduler is nil or not started).
func (s *Scheduler) Next() time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

func (s *Scheduler) loop(ctx context.Context) {
	for {
		// Recompute from the wall clock each time: survives DST and sleep.
		next := s.NextAfter(time.Now())
		s.mu.Lock()
		s.next = next
		s.mu.Unlock()
		slog.Info("crawler: next scheduled crawl", "at", next.Format(time.RFC3339))

		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			s.fire()
		}
	}
}

func (s *Scheduler) fire() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("crawler: scheduled crawl panicked", "panic", r)
		}
	}()
	for _, h := range s.Hooks { // independent sources, independent runners
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("crawler: scheduled hook panicked", "panic", r)
				}
			}()
			h()
		}()
	}
	if _, err := s.Engine.Start("cron", s.Engine.Cfg.Defaults); err != nil {
		slog.Warn("crawler: scheduled crawl not started", "err", err)
		return
	}
	// Then advance the full catalog (resumable, bounded by CATALOG_*).
	if base.EnvBool("CATALOG_DAILY", true) {
		s.Engine.Wait()
		if _, err := s.Engine.StartCatalog("cron", CatalogDefaultsFromEnv()); err != nil {
			slog.Warn("crawler: scheduled catalog crawl not started", "err", err)
		}
	}
}
