package chart

import (
	"sort"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/song"
)

// Lists are the crawlable howwe.ug lists. Path is the first listing page;
// paginated lists continue at Path + "/page/N".
type ListSource struct {
	Path      string
	Paginated bool
	// NewestFirst means new items push old ones down, so if page 1 is
	// unchanged every later page is unchanged too and can be skipped.
	NewestFirst bool
}

var Lists = map[string]ListSource{
	"hot100":        {Path: "/hot100"},
	"audio-new":     {Path: "/audio/new", Paginated: true, NewestFirst: true},
	"audio-day":     {Path: "/audio/day", Paginated: true},
	"audio-week":    {Path: "/audio/week", Paginated: true},
	"audio-month":   {Path: "/audio/month", Paginated: true},
	"audio-year":    {Path: "/audio/year", Paginated: true},
	"audio-alltime": {Path: "/audio/alltime", Paginated: true},
}

func ListNames() []string {
	names := make([]string, 0, len(Lists))
	for k := range Lists {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Snapshot is one distinct version of a list. A crawl that finds the same
// content as the latest snapshot only moves LastSeenAt forward, so the
// table grows only when the list actually changes.
type Snapshot struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	List        string    `json:"list" gorm:"size:32;not null;index:idx_snapshot_list_seen,priority:1"`
	Hash        string    `json:"-" gorm:"size:32;not null"`
	Pages       int       `json:"pages"`
	EntryCount  int       `json:"entryCount"`
	FirstSeenAt time.Time `json:"firstSeenAt" gorm:"not null;index:idx_snapshot_list_seen,priority:2"`
	LastSeenAt  time.Time `json:"lastSeenAt" gorm:"not null"`
	Entries     []Entry   `json:"-"`
}

type Entry struct {
	ID           uint       `json:"-" gorm:"primaryKey"`
	SnapshotID   uint       `json:"-" gorm:"not null;uniqueIndex:idx_entry_snapshot_pos,priority:1"`
	Position     int        `json:"position" gorm:"not null;uniqueIndex:idx_entry_snapshot_pos,priority:2"`
	SongID       uint       `json:"songId" gorm:"not null;index"`
	Song         *song.Song `json:"song,omitempty" gorm:"foreignKey:SongID"`
	Trend        string     `json:"trend,omitempty" gorm:"size:8"`
	LastDay      *int       `json:"lastDay,omitempty"`
	LastWeek     *int       `json:"lastWeek,omitempty"`
	Peak         *int       `json:"peak,omitempty"`
	WeeksOnChart *int       `json:"weeksOnChart,omitempty"`
}
