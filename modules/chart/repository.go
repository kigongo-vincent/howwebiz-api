package chart

import (
	"errors"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"gorm.io/gorm"
)

type entryFingerprint struct {
	Pos                 int
	SongID              int64
	Trend               string
	LD, LW, Peak, Weeks *int
}

// Hash fingerprints a list's ordering and chart stats (not cover URLs or
// other listing cosmetics).
func Hash(entries []scraper.ChartEntry) string {
	fp := make([]entryFingerprint, len(entries))
	for i, e := range entries {
		fp[i] = entryFingerprint{e.Position, e.Song.ID, e.Trend, e.LastDay, e.LastWeek, e.Peak, e.WeeksOnChart}
	}
	return scraper.Fingerprint(fp)
}

func Latest(db *gorm.DB, list string) (*Snapshot, error) {
	var s Snapshot
	err := db.Where("list = ?", list).Order("first_seen_at DESC, id DESC").First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &s, err
}

// Touch marks the latest snapshot as still current.
func Touch(db *gorm.DB, id uint, now time.Time) error {
	return db.Model(&Snapshot{}).Where("id = ?", id).UpdateColumn("last_seen_at", now).Error
}

// Save stores entries as the list's current state. If they match the
// latest snapshot nothing new is written; created reports which happened.
func Save(db *gorm.DB, list string, pages int, entries []scraper.ChartEntry, now time.Time) (snap *Snapshot, created bool, err error) {
	hash := Hash(entries)
	latest, err := Latest(db, list)
	if err != nil {
		return nil, false, err
	}
	if latest != nil && latest.Hash == hash {
		if latest.Pages < pages { // same content, but now known to cover more pages
			latest.Pages = pages
			if err := db.Model(latest).UpdateColumn("pages", pages).Error; err != nil {
				return nil, false, err
			}
		}
		latest.LastSeenAt = now
		return latest, false, Touch(db, latest.ID, now)
	}

	snap = &Snapshot{List: list, Hash: hash, Pages: pages, EntryCount: len(entries), FirstSeenAt: now, LastSeenAt: now}
	for _, e := range entries {
		snap.Entries = append(snap.Entries, Entry{
			Position: e.Position, SongID: uint(e.Song.ID), Trend: e.Trend,
			LastDay: e.LastDay, LastWeek: e.LastWeek, Peak: e.Peak, WeeksOnChart: e.WeeksOnChart,
		})
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		return tx.Omit("Entries.Song").CreateInBatches(snap, 200).Error
	})
	return snap, true, err
}
