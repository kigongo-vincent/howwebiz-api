package song

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"gorm.io/gorm"
)

// Data-access used by the crawler. Every write path first compares
// fingerprints so an unchanged crawl costs reads, not writes.

type listingFingerprint struct {
	Title, Slug, Artist string
	ArtistID            int64
	ArtistSlug          string
}

func ListingHash(r scraper.SongRef) string {
	return scraper.Fingerprint(listingFingerprint{r.Title, r.Slug, r.Artist, r.ArtistID, r.ArtistSlug})
}

// isSongCover reports whether a URL is a song cover rather than an artist
// photo (audio listings show artist photos, the chart shows song covers).
func isSongCover(u string) bool { return strings.Contains(u, "/_sng_cvrs_/") }

type UpsertStats struct {
	Inserted, Updated, Unchanged int
}

// UpsertListings stores songs seen on listing pages. Existing songs are
// written only if their listing fingerprint changed or a better cover is
// available; everything else is a no-op.
func UpsertListings(db *gorm.DB, baseURL string, refs []scraper.SongRef, now time.Time) (UpsertStats, error) {
	var st UpsertStats
	if len(refs) == 0 {
		return st, nil
	}
	byID := make(map[uint]scraper.SongRef, len(refs))
	ids := make([]uint, 0, len(refs))
	for _, r := range refs {
		id := uint(r.ID)
		if _, dup := byID[id]; !dup {
			ids = append(ids, id)
		}
		// Prefer a ref carrying a song cover when the same song repeats.
		if prev, dup := byID[id]; !dup || (!isSongCover(prev.CoverURL) && isSongCover(r.CoverURL)) {
			if dup && r.Genre == "" {
				r.Genre = prev.Genre
			}
			byID[id] = r
		}
	}

	type existingRow struct {
		ID          uint
		ListingHash string
		CoverURL    string
		Genre       string
	}
	var rows []existingRow
	if err := db.Model(&Song{}).Select("id", "listing_hash", "cover_url", "genre").Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return st, err
	}
	existing := make(map[uint]existingRow, len(rows))
	for _, r := range rows {
		existing[r.ID] = r
	}

	var inserts []Song
	err := db.Transaction(func(tx *gorm.DB) error {
		for _, id := range ids {
			r := byID[id]
			hash := ListingHash(r)
			old, ok := existing[id]
			if !ok {
				inserts = append(inserts, Song{
					ID: id, Title: r.Title, Slug: r.Slug, Artist: r.Artist, ArtistID: uint(r.ArtistID),
					ArtistSlug: r.ArtistSlug, PageURL: baseURL + r.PagePath, CoverURL: r.CoverURL, Genre: r.Genre,
					ListingHash: hash, CreatedAt: now, UpdatedAt: now,
				})
				continue
			}
			updates := map[string]any{}
			if old.ListingHash != hash {
				updates["title"], updates["slug"], updates["artist"] = r.Title, r.Slug, r.Artist
				updates["artist_id"], updates["artist_slug"] = uint(r.ArtistID), r.ArtistSlug
				updates["page_url"], updates["listing_hash"] = baseURL+r.PagePath, hash
			}
			if r.CoverURL != "" && (old.CoverURL == "" || (!isSongCover(old.CoverURL) && isSongCover(r.CoverURL))) {
				updates["cover_url"] = r.CoverURL
			}
			// Genre is only known from genre pages; other listings leave it alone.
			if r.Genre != "" && r.Genre != old.Genre {
				updates["genre"] = r.Genre
			}
			if len(updates) == 0 {
				st.Unchanged++
				continue
			}
			updates["updated_at"] = now
			if err := tx.Model(&Song{}).Where("id = ?", id).Updates(updates).Error; err != nil {
				return err
			}
			st.Updated++
		}
		if len(inserts) > 0 {
			if err := tx.CreateInBatches(inserts, 200).Error; err != nil {
				return err
			}
		}
		return nil
	})
	st.Inserted = len(inserts)
	return st, err
}

// DueForDetail returns which of ids need their song page (re)visited.
func DueForDetail(db *gorm.DB, ids []uint, now time.Time) ([]uint, error) {
	var due []uint
	err := db.Model(&Song{}).
		Where("id IN ?", ids).
		Where("next_detail_check_at IS NULL OR next_detail_check_at <= ?", now).
		Order("id").Pluck("id", &due).Error
	return due, err
}

const maxBackoffShift = 3 // next check is at most TTL × 8

// SaveDetail records a fetched song page. It returns changed=false (and
// only bumps the check bookkeeping) when the page's data is unchanged.
func SaveDetail(db *gorm.DB, id uint, d scraper.SongDetail, ttl time.Duration, now time.Time) (changed bool, err error) {
	var cur Song
	if err := db.Select("id", "detail_hash", "detail_stable_streak").First(&cur, id).Error; err != nil {
		return false, err
	}
	hash := scraper.Fingerprint(d)
	if hash == cur.DetailHash {
		streak := cur.DetailStableStreak + 1
		next := now.Add(ttl << min(streak, maxBackoffShift))
		return false, db.Model(&Song{}).Where("id = ?", id).UpdateColumns(map[string]any{
			"detail_checked_at": now, "detail_stable_streak": streak, "next_detail_check_at": next,
		}).Error
	}
	next := now.Add(ttl)
	return true, db.Model(&Song{}).Where("id = ?", id).Updates(map[string]any{
		"audio_url": d.AudioURL, "file_size": d.FileSize, "added_date": d.AddedDate,
		"album": d.Album, "album_url": d.AlbumURL, "video_url": d.VideoURL,
		"rating": d.Rating, "votes": d.Votes,
		"detail_hash": hash, "detail_stable_streak": 0, "detail_checked_at": now,
		"detail_changed_at": now, "next_detail_check_at": next, "updated_at": now,
	}).Error
}

// ResetDetailSchedule makes the given songs due immediately (force crawl).
func ResetDetailSchedule(db *gorm.DB, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return db.Model(&Song{}).Where("id IN ?", ids).UpdateColumn("next_detail_check_at", nil).Error
}

func PageURL(db *gorm.DB, id uint) (string, error) {
	var u string
	err := db.Model(&Song{}).Where("id = ?", id).Pluck("page_url", &u).Error
	return u, err
}

var slugRe = regexp.MustCompile(`[^a-z0-9.]+`)

func slugify(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// SaveDiscovered stores a song found by probing its id (not via a listing)
// together with its already-fetched detail page.
func SaveDiscovered(db *gorm.DB, baseURL string, id uint, d scraper.SongDetail, ttl time.Duration, now time.Time) error {
	ref := d.Canonical
	if ref.ID == 0 {
		ref = scraper.SongRef{ID: int64(id), Title: d.Title, Artist: d.Artist, Slug: slugify(d.Title), ArtistSlug: slugify(d.Artist)}
		ref.PagePath = fmt.Sprintf("/song/%d/%s/0/%s", id, ref.Slug, ref.ArtistSlug)
	}
	if _, err := UpsertListings(db, baseURL, []scraper.SongRef{ref}, now); err != nil {
		return err
	}
	_, err := SaveDetail(db, id, d, ttl, now)
	return err
}

// DetailBacklog returns up to limit songs whose page is due, never-fetched
// first (newest songs first within each group). limit <= 0 means no limit.
func DetailBacklog(db *gorm.DB, limit int, now time.Time) ([]uint, error) {
	q := db.Model(&Song{}).
		Where("next_detail_check_at IS NULL OR next_detail_check_at <= ?", now).
		Order("next_detail_check_at IS NOT NULL, id DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var ids []uint
	return ids, q.Pluck("id", &ids).Error
}

func MaxID(db *gorm.DB) (uint, error) {
	var id *uint
	err := db.Model(&Song{}).Select("MAX(id)").Scan(&id).Error
	if id == nil {
		return 0, err
	}
	return *id, err
}

// KnownIDs returns the subset of [from, to] already stored.
func KnownIDs(db *gorm.DB, from, to uint) (map[uint]bool, error) {
	var ids []uint
	if err := db.Model(&Song{}).Where("id BETWEEN ? AND ?", from, to).Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	set := make(map[uint]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, nil
}

type CatalogCounts struct {
	Songs        int64 `json:"songs"`
	WithAudio    int64 `json:"withAudio"`
	DetailsDue   int64 `json:"detailsDue"`
	NeverFetched int64 `json:"neverFetched"`
}

func Counts(db *gorm.DB, now time.Time) (CatalogCounts, error) {
	var c CatalogCounts
	m := func() *gorm.DB { return db.Model(&Song{}) }
	if err := m().Count(&c.Songs).Error; err != nil {
		return c, err
	}
	m().Where("audio_url <> ''").Count(&c.WithAudio)
	m().Where("next_detail_check_at IS NULL OR next_detail_check_at <= ?", now).Count(&c.DetailsDue)
	m().Where("detail_checked_at IS NULL").Count(&c.NeverFetched)
	return c, nil
}
