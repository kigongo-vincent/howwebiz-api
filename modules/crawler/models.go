package crawler

import (
	"time"

	"gorm.io/datatypes"
)

// Options for one crawl.
type Options struct {
	Targets      []string `json:"targets"`
	Pages        int      `json:"pages"`        // listing pages per paginated target
	FetchDetails bool     `json:"fetchDetails"` // visit song pages (audio URL, album, rating...)
	Force        bool     `json:"force"`        // bypass every cache layer
}

// Stats shows how much work each cache layer saved.
type Stats struct {
	PagesFetched   int `json:"pagesFetched"`
	PagesUnchanged int `json:"pagesUnchanged"` // fetched, parsed data identical to last time → no writes
	PagesSkipped   int `json:"pagesSkipped"`   // not fetched at all (newest-first list unchanged)

	SongsSeen      int `json:"songsSeen"`
	SongsInserted  int `json:"songsInserted"`
	SongsUpdated   int `json:"songsUpdated"`
	SongsUnchanged int `json:"songsUnchanged"`

	SnapshotsCreated int `json:"snapshotsCreated"`
	SnapshotsReused  int `json:"snapshotsReused"`

	DetailsDue       int `json:"detailsDue"`
	DetailsNotDue    int `json:"detailsNotDue"` // skipped: checked recently (adaptive TTL)
	DetailsDeferred  int `json:"detailsDeferred"`
	DetailsFetched   int `json:"detailsFetched"`
	DetailsChanged   int `json:"detailsChanged"`
	DetailsUnchanged int `json:"detailsUnchanged"`

	// catalog runs
	Probed       int `json:"probed"`       // song ids tried directly (not in any listing)
	ProbeFound   int `json:"probeFound"`   // ...that turned out to be real songs
	ProbeSkipped int `json:"probeSkipped"` // known-missing ids not re-probed

	GenresCompleted int `json:"genresCompleted"`

	ErrorsTotal int `json:"errorsTotal"` // errors[] keeps only the first 100
}

type CrawlRun struct {
	ID         uint                        `json:"id" gorm:"primaryKey"`
	Kind       string                      `json:"kind" gorm:"size:64;not null;default:lists;index"` // lists | catalog | <source> | site:<provider>
	Trigger    string                      `json:"trigger" gorm:"size:16;not null"`                  // manual | cron | startup
	Status     string                      `json:"status" gorm:"size:16;not null;index"`
	Options    datatypes.JSON              `json:"options"`
	Stats      datatypes.JSONType[Stats]   `json:"stats"`
	Errors     datatypes.JSONSlice[string] `json:"errors"`
	StartedAt  time.Time                   `json:"startedAt"`
	FinishedAt *time.Time                  `json:"finishedAt"`
}

const (
	KindLists   = "lists"
	KindCatalog = "catalog"

	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusPartial   = "partial"
	StatusFailed    = "failed"
)

// PageFingerprint remembers what a listing URL contained last time, so an
// unchanged page costs one GET and zero writes.
type PageFingerprint struct {
	URL             string    `gorm:"primaryKey;size:512"`
	Hash            string    `gorm:"size:32;not null"`
	CheckedAt       time.Time `gorm:"not null"`
	ChangedAt       time.Time `gorm:"not null"`
	UnchangedStreak int       `gorm:"not null;default:0"`
}

// CatalogCursor is the resumable position of the full-catalog crawl.
//
//	genres → walk every genre on /genres, each to its last page. Genres
//	         cover the whole catalog (125k songs vs ~120k on /audio/new,
//	         including old songs no other listing shows) and give each
//	         song its genre.
//	sweep  → probe song ids 1..max that no genre listed (safety net)
//	synced → later runs: walk /audio/new from page 1 until a page has
//	         nothing new (it is newest-first), sweep ids above the old
//	         max, and re-walk the genres every CATALOG_GENRE_REFRESH
type CatalogCursor struct {
	Name               string     `json:"-" gorm:"primaryKey;size:32"`
	Phase              string     `json:"phase" gorm:"size:16;not null"`
	Genre              string     `json:"genre"`     // genre being walked, "" when no walk is in progress
	GenrePage          int        `json:"genrePage"` // next page of that genre
	GenresDone         int        `json:"genresDone"`
	GenresTotal        int        `json:"genresTotal"`
	GenreWalkStartedAt *time.Time `json:"genreWalkStartedAt"`
	GenreWalkDoneAt    *time.Time `json:"genreWalkDoneAt"`
	SweepNextID        uint       `json:"sweepNextId"`
	SweepMaxID         uint       `json:"sweepMaxId"`
	SweepDoneAt        *time.Time `json:"sweepDoneAt"`
	LastSyncAt         *time.Time `json:"lastSyncAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

const (
	PhaseGenres = "genres"
	PhaseSweep  = "sweep"
	PhaseSynced = "synced"
)

// SongProbe remembers ids that resolved to no song, so sweeps don't
// re-request them every run (re-checked after ProbeRetryAfter).
type SongProbe struct {
	ID        uint      `gorm:"primaryKey;autoIncrement:false"`
	CheckedAt time.Time `gorm:"not null"`
}
