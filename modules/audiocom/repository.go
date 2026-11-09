package audiocom

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/scraper"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type UpsertStats struct{ Inserted, Updated, Unchanged int }

// audioResults extracts the audio models from search results.
func audioResults(rs []SearchResult) []apiAudio {
	var out []apiAudio
	for _, r := range rs {
		if r.Type != "audio" {
			continue
		}
		b, err := json.Marshal(r.Model)
		if err != nil {
			continue
		}
		var a apiAudio
		if json.Unmarshal(b, &a) == nil && a.ID != "" {
			out = append(out, a)
		}
	}
	return out
}

func toTrack(a apiAudio) Track {
	tags := a.Tags
	if tags == nil {
		tags = []string{}
	}
	lower := make([]string, len(tags))
	for i, t := range tags {
		lower[i] = strings.ToLower(strings.TrimPrefix(t, "#"))
	}
	return Track{
		ID: a.ID, Title: a.Title, Slug: a.Slug, Username: a.Username, AuthorName: a.AuthorName,
		Description: a.Description, Category: a.Category, License: a.License,
		Tags: datatypes.NewJSONSlice(tags), TagsIndex: "," + strings.Join(lower, ",") + ",",
		ImageURL: a.Image, PlayURL: a.Play.URL, WaveformURL: a.Play.Waveform, DurationMs: a.Play.Duration,
		Downloadable: a.IsDownload,
		CreatedAt:    time.Unix(a.DateCreated, 0).UTC(),
		ModifiedAt:   time.Unix(a.DateUpdated, 0).UTC(),
		Hash:         scraper.Fingerprint(a),
	}
}

// Upsert stores tracks, writing only new ones and ones whose data changed
// (fingerprint of the API model).
func Upsert(db *gorm.DB, audios []apiAudio, now time.Time) (UpsertStats, error) {
	var st UpsertStats
	if len(audios) == 0 {
		return st, nil
	}
	byID := map[string]Track{}
	ids := []string{}
	for _, a := range audios {
		if _, dup := byID[a.ID]; !dup {
			ids = append(ids, a.ID)
		}
		byID[a.ID] = toTrack(a)
	}
	var rows []struct {
		ID   string
		Hash string
	}
	if err := db.Model(&Track{}).Select("id", "hash").Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return st, err
	}
	have := map[string]string{}
	for _, r := range rows {
		have[r.ID] = r.Hash
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		var inserts []Track
		for _, id := range ids {
			t := byID[id]
			old, ok := have[id]
			switch {
			case !ok:
				t.FirstSeenAt, t.UpdatedAt = now, now
				inserts = append(inserts, t)
			case old == t.Hash:
				st.Unchanged++
			default:
				t.UpdatedAt = now
				if err := tx.Model(&Track{}).Where("id = ?", id).Select("*").Omit("id", "first_seen_at").Updates(&t).Error; err != nil {
					return err
				}
				st.Updated++
			}
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
