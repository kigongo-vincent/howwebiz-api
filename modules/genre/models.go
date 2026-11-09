package genre

import "time"

// Genre mirrors an entry of howwe.ug's /genres index.
type Genre struct {
	Name      string    `json:"name" gorm:"primaryKey;size:64"` // URL name, e.g. "AfroBeat"
	Title     string    `json:"title"`
	Path      string    `json:"path"`
	SiteCount int       `json:"siteCount"`      // songs howwe.ug reports
	Songs     int64     `json:"songs" gorm:"-"` // songs stored here
	UpdatedAt time.Time `json:"updatedAt"`
}
