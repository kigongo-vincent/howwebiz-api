package catalog

import (
	"time"

	"gorm.io/datatypes"
)

// Song is one aggregated catalog entry. Display fields come from the
// highest-priority provider that has them (CATALOG_PROVIDER_PRIORITY).
type Song struct {
	ID     uint   `json:"id" gorm:"primaryKey"`
	Key    string `json:"-" gorm:"size:400;uniqueIndex;not null"`
	Title  string `json:"title"`
	Artist string `json:"artist" gorm:"index"`
	// SearchText is every searchable field folded into one string (see
	// SearchText()), so a text query is one trigram-indexed LIKE per word.
	SearchText string `json:"-" gorm:"type:text;not null;default:''"`
	// SearchVersion is the SearchTextVersion search_text was built with.
	SearchVersion int16                       `json:"-" gorm:"not null;default:0"`
	Featured      datatypes.JSONSlice[string] `json:"featured"`
	Genre         string                      `json:"genre" gorm:"size:64;index"`
	CoverURL      string                      `json:"coverUrl"`
	AudioURL      string                      `json:"audioUrl"` // best playable/downloadable source
	DurationMs    int                         `json:"durationMs"`
	Plays         int64                       `json:"plays"` // sum over providers that report plays
	Providers     datatypes.JSONSlice[string] `json:"providers"`
	SourceCount   int                         `json:"sourceCount"`
	ReleasedAt    *time.Time                  `json:"releasedAt"`
	FirstSeenAt   time.Time                   `json:"firstSeenAt" gorm:"index"`
	UpdatedAt     time.Time                   `json:"updatedAt"`
	Sources       []Source                    `json:"sources" gorm:"foreignKey:SongID"`
}

func (Song) TableName() string { return "catalog_songs" }

// Source is one provider's copy of a song.
type Source struct {
	ID                uint       `json:"-" gorm:"primaryKey"`
	SongID            uint       `json:"-" gorm:"index;not null"`
	Provider          string     `json:"provider" gorm:"size:32;not null;uniqueIndex:idx_source_ref,priority:1"`
	ExternalID        string     `json:"externalId" gorm:"size:128;not null;uniqueIndex:idx_source_ref,priority:2"`
	Title             string     `json:"title"`
	Artist            string     `json:"artist"`
	AudioURL          string     `json:"audioUrl"`
	CoverURL          string     `json:"coverUrl"`
	PageURL           string     `json:"pageUrl"`
	DurationMs        int        `json:"durationMs"`
	Plays             *int       `json:"plays"`
	Genre             string     `json:"genre"`
	ReleasedAt        *time.Time `json:"releasedAt"`
	Hash              string     `json:"-" gorm:"size:32"`
	ProviderUpdatedAt time.Time  `json:"providerUpdatedAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

func (Source) TableName() string { return "catalog_sources" }

// Watermark is the Indexer's position in a provider Store.
type Watermark struct {
	Provider  string    `gorm:"primaryKey;size:32"`
	Since     time.Time `gorm:"not null"`
	AfterID   string    `gorm:"size:128"`
	Indexed   int64     // tracks indexed so far
	UpdatedAt time.Time
}

func (Watermark) TableName() string { return "catalog_watermarks" }
