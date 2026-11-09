package catalog

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Indexer pulls changed tracks from every provider Store into the catalog,
// incrementally (a watermark per provider), on an interval and on demand.
type Indexer struct {
	DB       *gorm.DB
	Ingester *Ingester
	Stores   []Store
	Batch    int

	mu      sync.Mutex // one pass at a time
	lastRun time.Time
}

type IndexResult struct {
	Provider string      `json:"provider"`
	Stats    IngestStats `json:"stats"`
	Error    string      `json:"error,omitempty"`
}

// RunOnce indexes everything changed since the last pass.
func (ix *Indexer) RunOnce(ctx context.Context) []IndexResult {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	batch := max(ix.Batch, 50)
	var out []IndexResult
	for _, s := range ix.Stores {
		res := IndexResult{Provider: s.Name()}
		wm := Watermark{Provider: s.Name()}
		ix.DB.Where("provider = ?", s.Name()).Limit(1).Find(&wm)
		for ctx.Err() == nil {
			tracks, err := s.Changed(ctx, wm.Since, wm.AfterID, batch)
			if err != nil {
				res.Error = err.Error()
				break
			}
			if len(tracks) == 0 {
				break
			}
			st, err := ix.Ingester.Ingest(tracks, time.Now().UTC())
			res.Stats.add(st)
			if err != nil {
				res.Error = err.Error()
				break
			}
			last := tracks[len(tracks)-1]
			wm.Since, wm.AfterID, wm.Indexed = last.UpdatedAt, last.ExternalID, wm.Indexed+int64(len(tracks))
			wm.UpdatedAt = time.Now().UTC()
			if err := ix.DB.Clauses(clause.OnConflict{UpdateAll: true}).Create(&wm).Error; err != nil {
				res.Error = err.Error()
				break
			}
			if len(tracks) < batch {
				break
			}
		}
		if res.Stats != (IngestStats{}) || res.Error != "" {
			slog.Info("catalog: indexed", "provider", res.Provider, "stats", res.Stats, "error", res.Error)
		}
		out = append(out, res)
	}
	ix.lastRun = time.Now().UTC()
	return out
}

// Start runs RunOnce every interval until ctx is cancelled.
func (ix *Indexer) Start(ctx context.Context, interval time.Duration) {
	go func() {
		ix.RunOnce(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ix.RunOnce(ctx)
			}
		}
	}()
}

func (ix *Indexer) LastRun() time.Time {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.lastRun
}
