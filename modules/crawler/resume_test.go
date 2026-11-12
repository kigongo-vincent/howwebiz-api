package crawler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func runsDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&CrawlRun{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func getRun(t *testing.T, db *gorm.DB, id uint) CrawlRun {
	var r CrawlRun
	if err := db.First(&r, id).Error; err != nil {
		t.Fatal(err)
	}
	return r
}

type testOpts struct {
	MaxPages int  `json:"maxPages"`
	Reset    bool `json:"reset"`
}

// The lifecycle of a safe restart: a shutdown interrupts a run, the next
// process resumes it with the same options (never "start over").
func TestShutdownInterruptsAndNextStartResumes(t *testing.T) {
	db := runsDB(t)
	ctx, shutdown := context.WithCancel(context.Background())
	r1 := NewRunner(ctx, db, slog.Default(), "test")
	started := make(chan struct{})
	run, err := r1.Launch("manual", "test", testOpts{MaxPages: 7, Reset: true}, func(ctx context.Context, st *RunState) {
		st.Add(func(s *Stats) { s.PagesFetched = 3 })
		close(started)
		<-ctx.Done() // a crawl stops at its next safe point
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	shutdown()
	r1.Wait()
	old := getRun(t, db, run.ID)
	if old.Status != StatusInterrupted || old.Stats.Data().PagesFetched != 3 {
		t.Fatalf("after shutdown: status=%s stats=%+v", old.Status, old.Stats.Data())
	}

	// next process
	r2 := NewRunner(context.Background(), db, slog.Default(), "test")
	var got testOpts
	r2.OnResume("test", func(trigger string, raw json.RawMessage) (*CrawlRun, error) {
		if trigger != TriggerResume {
			t.Errorf("trigger %q", trigger)
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return r2.Launch(trigger, "test", got, func(context.Context, *RunState) {})
	})
	resumed, err := r2.ResumeInterrupted()
	if err != nil || resumed == nil {
		t.Fatalf("resumed=%v err=%v", resumed, err)
	}
	r2.Wait()
	if got.MaxPages != 7 || got.Reset {
		t.Fatalf("resumed with %+v: want the same budget and reset off", got)
	}
	if n := getRun(t, db, resumed.ID); n.Attempt != 1 || n.Trigger != TriggerResume || n.Status != StatusSucceeded {
		t.Fatalf("new run: %+v", n)
	}
	if o := getRun(t, db, run.ID); o.ResumedBy == nil || *o.ResumedBy != resumed.ID {
		t.Fatalf("old run not linked: %+v", o.ResumedBy)
	}
	// nothing left to resume
	if again, _ := r2.ResumeInterrupted(); again != nil {
		t.Fatalf("resumed twice: run %d", again.ID)
	}
}

// A crash (or SIGKILL) leaves the run "running": the next start resumes
// the newest one and supersedes older leftovers.
func TestCrashedRunIsResumed(t *testing.T) {
	db := runsDB(t)
	now := time.Now().UTC()
	older := CrawlRun{Kind: "test", Trigger: "cron", Status: StatusInterrupted, StartedAt: now.Add(-time.Hour),
		Options: datatypes.JSON(`{"maxPages":1}`), Errors: datatypes.NewJSONSlice([]string{})}
	crashed := CrawlRun{Kind: "test", Trigger: "manual", Status: StatusRunning, StartedAt: now,
		Options: datatypes.JSON(`{"maxPages":5,"reset":true}`), Errors: datatypes.NewJSONSlice([]string{})}
	db.Create(&older)
	db.Create(&crashed)

	r := NewRunner(context.Background(), db, slog.Default(), "test")
	var got testOpts
	r.OnResume("test", func(trigger string, raw json.RawMessage) (*CrawlRun, error) {
		_ = json.Unmarshal(raw, &got)
		return r.Launch(trigger, "test", got, func(context.Context, *RunState) {})
	})
	resumed, err := r.ResumeInterrupted()
	if err != nil || resumed == nil {
		t.Fatalf("resumed=%v err=%v", resumed, err)
	}
	r.Wait()
	if got.MaxPages != 5 || got.Reset {
		t.Fatalf("resumed the wrong run or kept reset: %+v", got)
	}
	if o := getRun(t, db, older.ID); o.Status != StatusInterrupted || o.FinishedAt == nil {
		t.Fatalf("older leftover: %+v", o)
	}
	if c := getRun(t, db, crashed.ID); c.Status != StatusInterrupted || c.ResumedBy == nil {
		t.Fatalf("crashed run: %+v", c)
	}
}

// A run that keeps dying is not resumed forever.
func TestResumeGivesUpAfterMaxResumes(t *testing.T) {
	db := runsDB(t)
	stuck := CrawlRun{Kind: "test", Trigger: TriggerResume, Status: StatusRunning, StartedAt: time.Now().UTC(),
		Attempt: MaxResumes, Options: datatypes.JSON(`{}`), Errors: datatypes.NewJSONSlice([]string{})}
	db.Create(&stuck)
	r := NewRunner(context.Background(), db, slog.Default(), "test")
	called := false
	r.OnResume("test", func(string, json.RawMessage) (*CrawlRun, error) { called = true; return nil, nil })
	if run, err := r.ResumeInterrupted(); run != nil || err != nil || called {
		t.Fatalf("run=%v err=%v called=%v", run, err, called)
	}
	if s := getRun(t, db, stuck.ID); s.Status != StatusFailed {
		t.Fatalf("status %s", s.Status)
	}
}

func TestWithoutRestart(t *testing.T) {
	out := string(withoutRestart([]byte(`{"maxPages":3,"reset":true,"refresh":true,"refreshCategories":true,"force":true}`)))
	var m map[string]any
	_ = json.Unmarshal([]byte(out), &m)
	if m["reset"] != false || m["refresh"] != false || m["refreshCategories"] != false || m["force"] != true || m["maxPages"] != float64(3) {
		t.Fatalf("%s", out)
	}
}
