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

	// resumers relaunch an interrupted run of their kind from its stored
	// options (see OnResume, ResumeInterrupted).
	resumers map[string]Resumer
}

// Resumer starts a run of its kind from stored options (JSON), with the
// trigger "resume". Each source registers one for each kind it runs.
type Resumer func(trigger string, opts json.RawMessage) (*CrawlRun, error)

// MaxResumes: a run resumed this many times in a row (it keeps dying) is
// given up instead of resumed again.
const MaxResumes = 3

// TriggerResume marks runs started by ResumeInterrupted.
const TriggerResume = "resume"

// OnResume registers how to restart an interrupted run of kind.
func (e *Runner) OnResume(kind string, fn Resumer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.resumers == nil {
		e.resumers = map[string]Resumer{}
	}
	e.resumers[kind] = fn
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
	// Runs of this runner still marked running belong to a process that
	// died; they become interrupted (ResumeInterrupted continues them at
	// start; one left over now is simply recorded as such).
	e.DB.Model(&CrawlRun{}).Where("status = ? AND kind IN ?", StatusRunning, e.kinds).
		Update("status", StatusInterrupted)
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

	// At most CRAWL_CONCURRENCY crawls work at once, across all sources:
	// started together (every deploy starts them all), they saturated a
	// small database. The others wait here for a slot.
	if release, ok := acquireSlot(ctx, log); ok {
		// A panic (e.g. a parser meeting markup it never saw) fails this run
		// instead of killing the process and every other crawl with it.
		if err := base.Safe(run.Kind+" crawl", func() { job(ctx, st) }); err != nil {
			st.fail("%v", err)
		}
		release()
	}
	close(done)

	stats, _ := st.snapshot()
	didWork := stats.PagesFetched+stats.DetailsFetched+stats.Probed > 0
	status := StatusSucceeded
	switch {
	case ctx.Err() != nil:
		// shutdown: progress is saved (cursors, ingested songs); the next
		// start resumes this run where it stopped
		status = StatusInterrupted
		log.Info("crawl interrupted by shutdown; resumes on next start")
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

// ResumeInterrupted continues, at start, the latest run of this runner
// that a shutdown or crash stopped (status interrupted, or still running
// from a dead process). It is relaunched with its own options, minus the
// "start over" ones (reset, refresh): every source resumes from the
// cursors it saved as it went. Older interrupted runs are marked
// superseded; a run already resumed MaxResumes times in a row is given up.
// Call it once, before anything else launches runs.
func (e *Runner) ResumeInterrupted() (*CrawlRun, error) {
	var stopped []CrawlRun
	if err := e.DB.Where("kind IN ? AND status IN ? AND resumed_by IS NULL", e.kinds, []string{StatusRunning, StatusInterrupted}).
		Order("id DESC").Find(&stopped).Error; err != nil {
		return nil, err
	}
	if len(stopped) == 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	finish := func(r CrawlRun, status, why string) {
		errs := append([]string(r.Errors), why)
		upd := map[string]any{"status": status, "errors": datatypes.NewJSONSlice(errs)}
		if r.FinishedAt == nil {
			upd["finished_at"] = now
		}
		e.DB.Model(&CrawlRun{}).Where("id = ?", r.ID).Updates(upd)
	}
	for _, old := range stopped[1:] {
		finish(old, StatusInterrupted, fmt.Sprintf("superseded: run %d is the one resumed", stopped[0].ID))
	}
	last := stopped[0]
	if last.Attempt >= MaxResumes {
		finish(last, StatusFailed, fmt.Sprintf("not resumed: interrupted %d times in a row", last.Attempt+1))
		return nil, nil
	}
	e.mu.Lock()
	resume := e.resumers[last.Kind]
	e.mu.Unlock()
	if resume == nil {
		finish(last, StatusInterrupted, "not resumed: no resumer for kind "+last.Kind)
		return nil, nil
	}
	finish(last, StatusInterrupted, "resumed on start")
	run, err := resume(TriggerResume, withoutRestart(last.Options))
	if err != nil {
		return nil, fmt.Errorf("resume run %d (%s): %w", last.ID, last.Kind, err)
	}
	e.DB.Model(&CrawlRun{}).Where("id = ?", run.ID).Update("attempt", last.Attempt+1)
	e.DB.Model(&CrawlRun{}).Where("id = ?", last.ID).Update("resumed_by", run.ID)
	e.log.Info("crawl resumed", "run", run.ID, "kind", last.Kind, "from", last.ID, "attempt", last.Attempt+1)
	return run, nil
}

// withoutRestart turns off options that would discard saved progress.
func withoutRestart(opts []byte) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(opts, &m) != nil || m == nil {
		return json.RawMessage(opts)
	}
	for _, k := range []string{"reset", "refresh", "refreshCategories"} {
		if _, ok := m[k]; ok {
			m[k] = false
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(opts)
	}
	return b
}

// MarkInterrupted records this runner's in-progress run as interrupted
// (shutdown grace ran out before it stopped on its own).
func (e *Runner) MarkInterrupted() {
	e.DB.Model(&CrawlRun{}).Where("status = ? AND kind IN ?", StatusRunning, e.kinds).Updates(map[string]any{
		"status": StatusInterrupted, "finished_at": time.Now().UTC()})
}

var (
	slotsOnce sync.Once
	slots     chan struct{}
)

// acquireSlot waits for one of the CRAWL_CONCURRENCY (default 3) crawl
// slots; false when ctx ends first (shutdown: the run is interrupted).
func acquireSlot(ctx context.Context, log *slog.Logger) (release func(), ok bool) {
	slotsOnce.Do(func() { slots = make(chan struct{}, max(1, base.EnvInt("CRAWL_CONCURRENCY", 3))) })
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	default:
	}
	log.Info("crawl waiting for a free slot", "concurrency", cap(slots))
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	case <-ctx.Done():
		return func() {}, false
	}
}
