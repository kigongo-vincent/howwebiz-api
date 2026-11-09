// Package audiocom is the audio.com source. audio.com's website sits behind
// a Cloudflare bot challenge, so this module uses the official public API
// (https://api.audio.com, OpenAPI at /v1/openapi.json): OAuth 2.1 + PKCE,
// and GET /v1/search, which covers all listed audio — text search, `#tag`
// exact matches (genre-like browsing), and an empty query that browses
// everything newest first.
package audiocom

import (
	"time"

	"gorm.io/datatypes"
)

// Track is a listed audio on audio.com.
type Track struct {
	ID           string                      `json:"id" gorm:"primaryKey;size:64"`
	Title        string                      `json:"title"`
	Slug         string                      `json:"slug"`
	Username     string                      `json:"username" gorm:"size:128;index"`
	AuthorName   string                      `json:"authorName"`
	Description  string                      `json:"description"`
	Category     string                      `json:"category" gorm:"size:32;index"` // music, podcast, beats, ...
	License      string                      `json:"license" gorm:"size:16"`
	Tags         datatypes.JSONSlice[string] `json:"tags"`
	TagsIndex    string                      `json:"-"` // ",rock,live," for exact tag filtering
	ImageURL     string                      `json:"imageUrl"`
	PlayURL      string                      `json:"playUrl"` // streamable mp3
	WaveformURL  string                      `json:"waveformUrl"`
	DurationMs   int                         `json:"durationMs"`
	Downloadable bool                        `json:"downloadable"`
	CreatedAt    time.Time                   `json:"createdAt" gorm:"index"` // on audio.com
	ModifiedAt   time.Time                   `json:"modifiedAt"`             // on audio.com

	Hash        string    `json:"-" gorm:"size:32"`
	FirstSeenAt time.Time `json:"firstSeenAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func (Track) TableName() string { return "audiocom_tracks" }

// Auth is the single audio.com connection: the dynamically registered
// OAuth client and the tokens of the account that authorized it.
type Auth struct {
	Name         string `gorm:"primaryKey;size:16"` // always "default"
	ClientID     string `gorm:"size:256"`
	ClientSecret string `gorm:"size:256"`
	RedirectURI  string `gorm:"size:512"`
	AccessToken  string `gorm:"type:text"`
	RefreshToken string `gorm:"type:text"`
	ExpiresAt    *time.Time
	ConnectedAt  *time.Time
	// In-flight authorization (PKCE).
	PendingState    string `gorm:"size:128"`
	PendingVerifier string `gorm:"size:128"`
	UpdatedAt       time.Time
}

func (Auth) TableName() string { return "audiocom_auth" }

// Feed is the resumable position of one crawl feed: "all" (empty query,
// newest first) or "#tag".
type Feed struct {
	Name       string     `json:"name" gorm:"primaryKey;size:128"`
	NextPage   int        `json:"nextPage"`
	Complete   bool       `json:"complete"` // walked to the last page at least once
	WalkDoneAt *time.Time `json:"walkDoneAt"`
	LastSyncAt *time.Time `json:"lastSyncAt"`
	Tracks     int        `json:"tracksSeenLastWalk"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

func (Feed) TableName() string { return "audiocom_feeds" }

// ---- API shapes (subset of the OpenAPI schemas) ----

type apiAudio struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Slug        string   `json:"slug"`
	Username    string   `json:"username"`
	AuthorName  string   `json:"author_name"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	License     string   `json:"license"`
	Tags        []string `json:"tags"`
	Image       string   `json:"image"`
	IsListed    bool     `json:"is_listed"`
	IsDownload  bool     `json:"is_downloadable"`
	DateCreated int64    `json:"date_created"`
	DateUpdated int64    `json:"date_updated"`
	Play        struct {
		URL      string `json:"url"`
		Duration int    `json:"duration"`
		Waveform string `json:"waveform"`
	} `json:"play"`
}

// SearchResult mirrors the API: model's shape depends on type
// (audio | author | collection).
type SearchResult struct {
	Type  string         `json:"type"`
	Model map[string]any `json:"model"`
}
