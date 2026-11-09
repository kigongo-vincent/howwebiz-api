package chart

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"gorm.io/gorm"
)

type ChartService struct {
	DB  *gorm.DB
	Loc *time.Location // timezone used to interpret ?date=
}

func NewService(db *gorm.DB) *ChartService {
	return &ChartService{DB: db, Loc: base.Location()}
}

type listSummary struct {
	Name   string    `json:"name"`
	Latest *Snapshot `json:"latest"`
}

// Index lists every crawlable list and its current snapshot.
func (s *ChartService) Index(c *fiber.Ctx) error {
	out := make([]listSummary, 0, len(Lists))
	for _, name := range ListNames() {
		snap, err := Latest(s.DB, name)
		if err != nil {
			return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get lists")
		}
		out = append(out, listSummary{Name: name, Latest: snap})
	}
	return c.JSON(base.APIResponse{Data: out})
}

// Show returns a page of a list's entries with their songs.
//
//	?date=YYYY-MM-DD  the list as it was at the end of that day (default: now)
func (s *ChartService) Show(c *fiber.Ctx) error {
	return s.showNamed(c, c.Params("list"))
}

// Hot100 is a shortcut for Show on the hot100 list.
func (s *ChartService) Hot100(c *fiber.Ctx) error {
	return s.showNamed(c, "hot100")
}

func (s *ChartService) showNamed(c *fiber.Ctx, list string) error {
	if _, ok := Lists[list]; !ok {
		return base.API_FAIL(c, fiber.StatusNotFound, "unknown list")
	}
	q := s.DB.Where("list = ?", list)
	if d := c.Query("date"); d != "" {
		day, err := time.ParseInLocation(time.DateOnly, d, s.Loc)
		if err != nil {
			return base.API_ERROR(c, "date must be YYYY-MM-DD")
		}
		q = q.Where("first_seen_at < ?", day.AddDate(0, 0, 1))
	}
	var snap Snapshot
	if err := q.Order("first_seen_at DESC, id DESC").Limit(1).Find(&snap).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get list")
	}
	if snap.ID == 0 {
		return base.API_FAIL(c, fiber.StatusNotFound, "no data for this list yet; start a crawl with POST /api/v1/crawl")
	}

	p := base.ParsePage(c)
	entries := []Entry{}
	if err := p.Apply(s.DB.Where("snapshot_id = ?", snap.ID)).
		Preload("Song").Order("position").Find(&entries).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get list entries")
	}
	return c.JSON(base.APIResponse{
		Data:       entries,
		Pagination: p.Controls(int64(snap.EntryCount), len(entries)),
		Meta:       fiber.Map{"list": list, "snapshot": snap},
	})
}

// History returns the distinct versions of a list, newest first.
func (s *ChartService) History(c *fiber.Ctx) error {
	list := c.Params("list")
	if _, ok := Lists[list]; !ok {
		return base.API_FAIL(c, fiber.StatusNotFound, "unknown list")
	}
	p := base.ParsePage(c)
	q := s.DB.Model(&Snapshot{}).Where("list = ?", list)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to count snapshots")
	}
	snaps := []Snapshot{}
	if err := p.Apply(q).Order("first_seen_at DESC, id DESC").Find(&snaps).Error; err != nil {
		return base.API_FAIL(c, fiber.StatusInternalServerError, "failed to get snapshots")
	}
	return c.JSON(base.APIResponse{Data: snaps, Pagination: p.Controls(total, len(snaps))})
}
