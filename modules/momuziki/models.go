package momuziki

import "time"

// Song is a momuziki.com song; ID is the site's song.php?id.
type Song struct {
	ID         uint   `json:"id" gorm:"primaryKey;autoIncrement:false"`
	Title      string `json:"title"`
	Artist     string `json:"artist" gorm:"index"`
	CategoryID uint   `json:"categoryId" gorm:"index"` // 0 = found by id sweep, no category seen
	PageURL    string `json:"pageUrl"`
	ThumbURL   string `json:"thumbUrl"`
	ImageURL   string `json:"imageUrl"`
	AudioURL   string `json:"audioUrl"`
	Plays      *int   `json:"plays"`

	ListingHash        string     `json:"-" gorm:"size:32"`
	DetailHash         string     `json:"-" gorm:"size:32"`
	DetailStableStreak int        `json:"-"`
	DetailCheckedAt    *time.Time `json:"detailCheckedAt"`
	NextDetailCheckAt  *time.Time `json:"-" gorm:"index"`
	FirstSeenAt        time.Time  `json:"firstSeenAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

func (Song) TableName() string { return "momuziki_songs" }

type Category struct {
	ID        uint      `json:"id" gorm:"primaryKey;autoIncrement:false"`
	Name      string    `json:"name"`
	ImageURL  string    `json:"imageUrl"`
	Songs     int64     `json:"songs" gorm:"-"` // stored songs in this category
	UpdatedAt time.Time `json:"updatedAt"`
}

func (Category) TableName() string { return "momuziki_categories" }

// Cursor is the resumable position of the full crawl.
//
//	categories → walk every category to its last page (resumable)
//	sweep      → probe song ids no category listed, from 1 to the highest
//	             known id and then onward until ProbeAhead misses in a row
//	             (uncategorized new uploads sit above the listed maximum)
//	synced     → later runs: each category from page 1 until a page adds
//	             nothing (categories list newest first), a periodic full
//	             re-walk, and the sweep continues above the known maximum
type Cursor struct {
	Name            string     `json:"-" gorm:"primaryKey;size:16"`
	Phase           string     `json:"phase" gorm:"size:16"`
	CategoryID      uint       `json:"categoryId"` // category being walked, 0 = none
	Page            int        `json:"page"`
	CategoriesDone  int        `json:"categoriesDone"`
	CategoriesTotal int        `json:"categoriesTotal"`
	WalkStartedAt   *time.Time `json:"walkStartedAt"`
	WalkDoneAt      *time.Time `json:"walkDoneAt"`
	SweepNextID     uint       `json:"sweepNextId"`
	SweepDoneAt     *time.Time `json:"sweepDoneAt"`
	LastSyncAt      *time.Time `json:"lastSyncAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

func (Cursor) TableName() string { return "momuziki_cursor" }

const (
	PhaseCategories = "categories"
	PhaseSweep      = "sweep"
	PhaseSynced     = "synced"
)

// Probe remembers ids that resolved to no song.
type Probe struct {
	ID        uint      `gorm:"primaryKey;autoIncrement:false"`
	CheckedAt time.Time `gorm:"not null"`
}

func (Probe) TableName() string { return "momuziki_probes" }
