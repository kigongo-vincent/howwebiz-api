package crawler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Runner owns the lifecycle of crawl runs for a set of kinds: one run at a
// time, a CrawlRun record, periodic progress flushes and a final status.
// Each source (howwe.ug, audio.com) has its own Runner, so a long crawl of
// one never blocks the other.
type Runner struct {
	DB    *gorm.DB
	ctx   context.Context
	log   *slog.Logger
	kinds []string

	mu      sync.Mutex
	running uint
	wg      sync.WaitGroup
}

func NewRunner(ctx context.Context, db *gorm.DB, log *slog.Logger, kinds ...string) *Runner {
	return &Runner{DB: db, ctx: ctx, log: log, kinds: kinds}
}

// Running returns the id of the in-progress run, or 0.
func (e *Runner) Running() uint {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// Wait blocks until an in-progress run has exited.
func (e *Runner) Wait() { e.wg.Wait() }

// launch is the shared run lifecycle: single-flight, run record, periodic
// progress flushes (long catalog runs are observable while running) and a
// final status that survives shutdown.
func (e *Runner) Launch(trigger, kind string, opts any, job func(ctx context.Context, st *RunState)) (*CrawlRun, error) {
	optJSON, err := json.Marshal(opts)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running != 0 {
		return &CrawlRun{ID: e.running}, ErrBusy
	}
	// Mark this runner's leftovers from a crashed process so history stays
	// truthful (other runners' kinds may legitimately be running).
	e.DB.Model(&CrawlRun{}).Where("status = ? AND kind IN ?", StatusRunning, e.kinds).Updates(map[string]any{
		"status": StatusFailed, "errors": datatypes.NewJSONSlice([]string{"interrupted: server stopped"}),
	})
	run := &CrawlRun{
		Kind: kind, Trigger: trigger, Status: StatusRunning, StartedAt: time.Now().UTC(),
		Options: datatypes.JSON(optJSON), Errors: datatypes.NewJSONSlice([]string{}),
	}
	if err := e.DB.Create(run).Error; err != nil {
		return nil, err
	}
	e.running = run.ID
	e.wg.Go(func() {
		defer func() {
			e.mu.Lock()
			e.running = 0
			e.mu.Unlock()
		}()
		e.execute(run, job)
	})
	return run, nil
}

const maxStoredErrors = 100

// RunState collects results from concurrent workers.
type RunState struct {
	mu    sync.Mutex
	stats Stats
	errs  []string
	seen  map[uint]bool
}

func (r *RunState) fail(format string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.ErrorsTotal++
	if len(r.errs) < maxStoredErrors {
		r.errs = append(r.errs, fmt.Sprintf(format, a...))
	}
}

func (r *RunState) add(f func(*Stats)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.stats)
}

func (r *RunState) see(ids ...uint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		r.seen[id] = true
	}
}

func (r *RunState) snapshot() (Stats, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats, append([]string{}, r.errs...)
}

func (e *Runner) execute(run *CrawlRun, job func(ctx context.Context, st *RunState)) {
	ctx := e.ctx
	log := e.log.With("run", run.ID, "kind", run.Kind)
	log.Info("crawl started", "trigger", run.Trigger, "options", string(run.Options))
	start := time.Now()
	st := &RunState{seen: map[uint]bool{}}

	flush := func(fields map[string]any) {
		stats, errs := st.snapshot()
		fields["stats"] = datatypes.NewJSONType(stats)
		fields["errors"] = datatypes.NewJSONSlice(errs)
		if err := e.DB.Model(run).Updates(fields).Error; err != nil {
			log.Error("record run progress", "err", err)
		}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				flush(map[string]any{})
			}
		}
	}()

	// A panic (e.g. a parser meeting markup it never saw) fails this run
	// instead of killing the process and every other crawl with it.
	if err := base.Safe(run.Kind+" crawl", func() { job(ctx, st) }); err != nil {
		st.fail("%v", err)
	}
	close(done)
	if ctx.Err() != nil {
		st.fail("cancelled: %v", ctx.Err())
	}

	stats, _ := st.snapshot()
	didWork := stats.PagesFetched+stats.DetailsFetched+stats.Probed > 0
	status := StatusSucceeded
	switch {
	case stats.ErrorsTotal > 0 && !didWork:
		status = StatusFailed
	case stats.ErrorsTotal > 0:
		status = StatusPartial
	}
	// ctx may be cancelled (shutdown); the DB write does not use it.
	flush(map[string]any{"status": status, "finished_at": time.Now().UTC()})
	log.Info("crawl finished", "status", status, "took", time.Since(start).Round(time.Millisecond),
		"pagesFetched", stats.PagesFetched, "pagesUnchanged", stats.PagesUnchanged, "pagesSkipped", stats.PagesSkipped,
		"songsInserted", stats.SongsInserted, "songsUpdated", stats.SongsUpdated,
		"probed", stats.Probed, "probeFound", stats.ProbeFound,
		"detailsFetched", stats.DetailsFetched, "detailsChanged", stats.DetailsChanged, "detailsNotDue", stats.DetailsNotDue,
		"errors", stats.ErrorsTotal)
}

// Exported helpers for jobs in other packages.

// Fail records an error (the run ends "partial", or "failed" if no work was done).
func (r *RunState) Fail(format string, a ...any) { r.fail(format, a...) }

// Add updates the run's counters.
func (r *RunState) Add(f func(*Stats)) { r.add(f) }
