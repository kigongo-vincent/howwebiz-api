package song

import "time"

// Song is a howwe.ug song. ID is howwe's own song id, so links stay stable.
//
// The *Hash / *Check* fields drive the crawl cache and are not exposed:
//   - ListingHash fingerprints the fields seen on listing pages; a song is
//     only re-written when that fingerprint changes.
//   - DetailHash fingerprints the song page; NextDetailCheckAt backs off
//     (TTL × 2^streak, capped) while the page keeps coming back unchanged.
type Song struct {
	ID         uint   `json:"id" gorm:"primaryKey;autoIncrement:false"`
	Title      string `json:"title" gorm:"not null"`
	Slug       string `json:"slug"`
	Artist     string `json:"artist" gorm:"not null"`
	ArtistID   uint   `json:"artistId" gorm:"index"`
	ArtistSlug string `json:"artistSlug"`
	PageURL    string `json:"pageUrl"`
	CoverURL   string `json:"coverUrl"`
	Genre      string `json:"genre" gorm:"size:64;index"` // from /genres pages

	AudioURL  string   `json:"audioUrl"`
	FileSize  string   `json:"fileSize"`
	AddedDate string   `json:"addedDate"`
	Album     string   `json:"album"`
	AlbumURL  string   `json:"albumUrl"`
	VideoURL  string   `json:"videoUrl"`
	Rating    *float64 `json:"rating"`
	Votes     *int     `json:"votes"`

	ListingHash        string     `json:"-" gorm:"size:32"`
	DetailHash         string     `json:"-" gorm:"size:32"`
	DetailStableStreak int        `json:"-" gorm:"not null;default:0"`
	DetailCheckedAt    *time.Time `json:"detailCheckedAt"`
	DetailChangedAt    *time.Time `json:"detailChangedAt"`
	NextDetailCheckAt  *time.Time `json:"-" gorm:"index"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
