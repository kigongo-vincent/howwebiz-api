package audiocom

import (
	"context"
	"errors"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
)

// CatalogStore exposes stored audio.com tracks to the catalog indexer.
type CatalogStore struct{ S *Service }

func (CatalogStore) Name() string { return "audiocom" }

func (a CatalogStore) Changed(ctx context.Context, since time.Time, afterID string, limit int) ([]catalog.Track, error) {
	var rows []Track
	err := a.S.DB.WithContext(ctx).
		Where("updated_at > ? OR (updated_at = ? AND id > ?)", since, since, afterID).
		Order("updated_at").Order("id").Limit(limit).Find(&rows).Error
	out := make([]catalog.Track, 0, len(rows))
	for _, r := range rows {
		out = append(out, trackToCatalog(r))
	}
	return out, err
}

func trackToCatalog(r Track) catalog.Track {
	artist := r.AuthorName
	if artist == "" {
		artist = r.Username
	}
	page := ""
	if r.Username != "" && r.Slug != "" {
		page = "https://audio.com/" + r.Username + "/audio/" + r.Slug
	}
	return catalog.Track{Provider: "audiocom", ExternalID: r.ID, Title: r.Title, Artist: artist,
		AudioURL: r.PlayURL, CoverURL: r.ImageURL, PageURL: page, DurationMs: r.DurationMs, UpdatedAt: r.UpdatedAt}
}

// CatalogSearcher runs audio.com's official search API for /songs.
// Unavailable until an account is connected.
type CatalogSearcher struct{ S *Service }

func (CatalogSearcher) Name() string { return "audiocom" }

func (a CatalogSearcher) Search(ctx context.Context, q string, limit int) ([]catalog.Track, error) {
	if !a.S.OAuth.Connected() {
		return nil, catalog.ErrUnavailable
	}
	res, err := a.S.Client.Search(ctx, q, "audio", 1)
	if errors.Is(err, ErrNotConnected) {
		return nil, catalog.ErrUnavailable
	}
	if err != nil {
		return nil, err
	}
	audios := audioResults(res)
	if _, err := Upsert(a.S.DB, audios, time.Now().UTC()); err != nil {
		return nil, err
	}
	out := make([]catalog.Track, 0, len(audios))
	for _, x := range audios[:min(len(audios), limit)] {
		out = append(out, trackToCatalog(toTrack(x)))
	}
	return out, nil
}
