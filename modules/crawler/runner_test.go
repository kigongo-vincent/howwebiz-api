package crawler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// A crawl job that panics must fail its own run and free the runner; before
// this guard the panic ended the whole process (and every other crawl).
func TestRunnerSurvivesPanickingJob(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&CrawlRun{}); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(context.Background(), db, slog.Default(), "test")

	run, err := r.Launch("manual", "test", nil, func(ctx context.Context, st *RunState) {
		var s []int
		_ = s[3] // index out of range
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Wait()

	var got CrawlRun
	if err := db.First(&got, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", got.Status, StatusFailed)
	}
	if errs := strings.Join(got.Errors, "; "); !strings.Contains(errs, "panic") {
		t.Fatalf("errors = %s, want the panic recorded", errs)
	}
	if r.Running() != 0 {
		t.Fatalf("runner still busy with run %d", r.Running())
	}
	// and the next run starts normally
	if _, err := r.Launch("manual", "test", nil, func(context.Context, *RunState) {}); err != nil {
		t.Fatalf("next launch: %v", err)
	}
	r.Wait()
}
