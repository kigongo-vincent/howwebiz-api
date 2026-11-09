package momuziki

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
)

// CatalogStore exposes momuziki songs to the catalog indexer.
type CatalogStore struct{ S *Service }

func (CatalogStore) Name() string { return "momuziki" }

func (a CatalogStore) Changed(ctx context.Context, since time.Time, afterID string, limit int) ([]catalog.Track, error) {
	after, _ := strconv.ParseUint(afterID, 10, 64)
	var rows []Song
	err := a.S.DB.WithContext(ctx).
		Where("updated_at > ? OR (updated_at = ? AND id > ?)", since, since, after).
		Order("updated_at").Order("id").Limit(limit).Find(&rows).Error
	var cats []Category
	a.S.DB.Select("id", "name").Find(&cats) // one query per batch, not per song
	names := make(map[uint]string, len(cats))
	for _, c := range cats {
		names[c.ID] = c.Name
	}
	out := make([]catalog.Track, 0, len(rows))
	for _, r := range rows {
		out = append(out, track(r, names[r.CategoryID]))
	}
	return out, err
}

func track(r Song, genre string) catalog.Track {
	cover := r.ImageURL
	if cover == "" {
		cover = r.ThumbURL
	}
	return catalog.Track{Provider: "momuziki", ExternalID: strconv.FormatUint(uint64(r.ID), 10), Title: r.Title,
		Artist: r.Artist, AudioURL: r.AudioURL, CoverURL: cover, PageURL: r.PageURL, Plays: r.Plays, Genre: genre,
		UpdatedAt: r.UpdatedAt}
}

// CatalogSearcher runs momuziki's own search page for /songs.
type CatalogSearcher struct{ S *Service }

func (CatalogSearcher) Name() string { return "momuziki" }

func (a CatalogSearcher) Search(ctx context.Context, q string, limit int) ([]catalog.Track, error) {
	body, err := a.S.Web.Get(ctx, "/search.php?"+url.Values{"query": {q}}.Encode())
	if fatal(err) {
		return nil, catalog.ErrUnavailable
	}
	if err != nil {
		return nil, err
	}
	rows, err := ParseRows(body, a.S.base)
	if err != nil {
		return nil, err
	}
	// Keep momuziki's own table in step (the indexer would pick these up
	// anyway; returning them now merges them into this search).
	if _, err := UpsertRows(a.S.DB, a.S.Cfg.Web.BaseURL, rows, 0, time.Now().UTC()); err != nil {
		return nil, err
	}
	out := make([]catalog.Track, 0, min(len(rows), limit))
	for _, r := range rows[:min(len(rows), limit)] {
		out = append(out, catalog.Track{Provider: "momuziki", ExternalID: strconv.FormatUint(uint64(r.ID), 10),
			Title: r.Title, Artist: r.Artist, AudioURL: r.AudioURL, CoverURL: r.ThumbURL,
			PageURL: a.S.Cfg.Web.BaseURL + "/song.php?id=" + strconv.FormatUint(uint64(r.ID), 10)})
	}
	return out, nil
}
