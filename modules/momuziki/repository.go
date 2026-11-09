package momuziki

import (
	"fmt"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"gorm.io/gorm"
)

type UpsertStats struct{ Inserted, Updated, Unchanged int }

func listingHash(r Row) string {
	return scraper.Fingerprint([]string{r.Title, r.Artist, r.ThumbURL, r.AudioURL})
}

// UpsertRows stores listing rows; only new or changed rows are written.
// categoryID 0 (search results) never overwrites a known category.
func UpsertRows(db *gorm.DB, baseURL string, rows []Row, categoryID uint, now time.Time) (UpsertStats, error) {
	var st UpsertStats
	if len(rows) == 0 {
		return st, nil
	}
	ids := make([]uint, 0, len(rows))
	byID := map[uint]Row{}
	for _, r := range rows {
		if _, dup := byID[r.ID]; !dup {
			ids = append(ids, r.ID)
		}
		byID[r.ID] = r
	}
	var have []struct {
		ID          uint
		ListingHash string
		CategoryID  uint
	}
	if err := db.Model(&Song{}).Select("id", "listing_hash", "category_id").Where("id IN ?", ids).Find(&have).Error; err != nil {
		return st, err
	}
	old := map[uint]int{}
	for i, h := range have {
		old[h.ID] = i
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		var inserts []Song
		for _, id := range ids {
			r := byID[id]
			hash := listingHash(r)
			i, ok := old[id]
			if !ok {
				inserts = append(inserts, Song{ID: id, Title: r.Title, Artist: r.Artist, CategoryID: categoryID,
					PageURL: fmt.Sprintf("%s/song.php?id=%d", baseURL, id), ThumbURL: r.ThumbURL, AudioURL: r.AudioURL,
					ListingHash: hash, FirstSeenAt: now, UpdatedAt: now})
				continue
			}
			upd := map[string]any{}
			if have[i].ListingHash != hash {
				upd["title"], upd["artist"], upd["thumb_url"], upd["audio_url"], upd["listing_hash"] = r.Title, r.Artist, r.ThumbURL, r.AudioURL, hash
			}
			if categoryID != 0 && have[i].CategoryID != categoryID {
				upd["category_id"] = categoryID
			}
			if len(upd) == 0 {
				st.Unchanged++
				continue
			}
			upd["updated_at"] = now
			if err := tx.Model(&Song{}).Where("id = ?", id).Updates(upd).Error; err != nil {
				return err
			}
			st.Updated++
		}
		if len(inserts) > 0 {
			if err := tx.CreateInBatches(inserts, 200).Error; err != nil {
				return err
			}
			st.Inserted = len(inserts)
		}
		return nil
	})
	return st, err
}

// SaveSongPage records song.php data; unchanged pages only move the
// adaptive revisit time (TTL × 2^streak, capped at 8×).
func SaveSongPage(db *gorm.DB, baseURL string, id uint, p SongPage, ttl time.Duration, now time.Time) (changed bool, err error) {
	hash := scraper.Fingerprint(p)
	var cur Song
	if err := db.Where("id = ?", id).Limit(1).Find(&cur).Error; err != nil {
		return false, err
	}
	if cur.ID == 0 { // found by the id sweep
		cur = Song{ID: id, Title: p.Title, Artist: p.Artist, PageURL: fmt.Sprintf("%s/song.php?id=%d", baseURL, id),
			AudioURL: p.AudioURL, ThumbURL: p.ImageURL, FirstSeenAt: now, UpdatedAt: now}
		if err := db.Create(&cur).Error; err != nil {
			return false, err
		}
	}
	if cur.DetailHash == hash {
		streak := cur.DetailStableStreak + 1
		next := now.Add(ttl << min(streak, 3))
		return false, db.Model(&Song{}).Where("id = ?", id).UpdateColumns(map[string]any{
			"detail_checked_at": now, "detail_stable_streak": streak, "next_detail_check_at": next}).Error
	}
	upd := map[string]any{"image_url": p.ImageURL, "plays": p.Plays, "detail_hash": hash, "detail_stable_streak": 0,
		"detail_checked_at": now, "next_detail_check_at": now.Add(ttl), "updated_at": now}
	if p.AudioURL != "" {
		upd["audio_url"] = p.AudioURL
	}
	if p.Title != "" {
		upd["title"], upd["artist"] = p.Title, p.Artist
	}
	return true, db.Model(&Song{}).Where("id = ?", id).Updates(upd).Error
}

func detailBacklog(db *gorm.DB, limit int, now time.Time) ([]uint, error) {
	q := db.Model(&Song{}).Where("next_detail_check_at IS NULL OR next_detail_check_at <= ?", now).
		Order("next_detail_check_at IS NOT NULL, id DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var ids []uint
	return ids, q.Pluck("id", &ids).Error
}

func maxID(db *gorm.DB) uint {
	var id *uint
	db.Model(&Song{}).Select("MAX(id)").Scan(&id)
	if id == nil {
		return 0
	}
	return *id
}
