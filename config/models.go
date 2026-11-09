package config

import (
	"github.com/gofiber/fiber/v2"
	"github.com/kigongo-vincent/howwebiz-api/modules/audiocom"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/momuziki"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
	"gorm.io/gorm"
)

type Dependencies struct {
	DB        *gorm.DB
	App       *fiber.App
	Crawler   *crawler.Engine
	Scheduler *crawler.Scheduler // nil when the daily crawl is disabled
	AudioCom  *audiocom.Service
	Momuziki  *momuziki.Service
	Catalog   *catalog.Service   // aggregated /songs
	Sites     sitecrawl.Registry // generic HTML-site providers
}
