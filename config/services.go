package config

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/etag"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/joho/godotenv"
	"github.com/kigongo-vincent/howwebiz-api/modules/audiocom"
	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"github.com/kigongo-vincent/howwebiz-api/modules/catalog"
	"github.com/kigongo-vincent/howwebiz-api/modules/chart"
	"github.com/kigongo-vincent/howwebiz-api/modules/crawler"
	"github.com/kigongo-vincent/howwebiz-api/modules/docs"
	"github.com/kigongo-vincent/howwebiz-api/modules/genre"
	"github.com/kigongo-vincent/howwebiz-api/modules/momuziki"
	"github.com/kigongo-vincent/howwebiz-api/modules/providers/sitecrawl"
	"github.com/kigongo-vincent/howwebiz-api/modules/search"
	"github.com/kigongo-vincent/howwebiz-api/modules/song"
	"github.com/kigongo-vincent/howwebiz-api/modules/webguard"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func RegisterAppRoutes(deps Dependencies) {
	if err := docs.Register(deps.App); err != nil {
		log.Fatalf("api docs: %v", err)
	}
	v1 := deps.App.Group("/api/v1")

	v1.Get("/health", func(c *fiber.Ctx) error {
		sqlDB, err := deps.DB.DB()
		if err != nil || sqlDB.Ping() != nil {
			return base.API_FAIL(c, fiber.StatusServiceUnavailable, "database unreachable")
		}
		return c.JSON(base.APIResponse{Message: "ok"})
	})

	register := base.RouteRegister{DB: deps.DB, Router: v1}
	song.RouterRegister(&register)
	genre.RouterRegister(&register)
	chart.RouterRegister(&register)
	search.RouterRegister(&register)
	crawler.RouterRegister(&register, deps.Crawler, deps.Scheduler)
	if deps.AudioCom != nil {
		audiocom.RouterRegister(&register, deps.AudioCom)
	}
	if deps.Momuziki != nil {
		momuziki.RouterRegister(&register, deps.Momuziki)
	}
	if deps.Catalog != nil {
		catalog.RouterRegister(&register, deps.Catalog)
	}
	if deps.Sites != nil {
		sitecrawl.RouterRegister(&register, deps.Sites)
	}
}

// LoadEnv loads ../.env relative to this file (repo root), then ./.env.
// Real environment variables always win.
func LoadEnv() {
	_, filename, _, _ := runtime.Caller(0)
	_ = godotenv.Load(filepath.Join(filepath.Dir(filename), "../.env"))
	_ = godotenv.Load()
}

func SetupApplication() *fiber.App {
	LoadEnv()
	app := fiber.New(fiber.Config{
		AppName:      "howwebiz-api",
		ErrorHandler: errorHandler,
	})
	app.Use(recover.New())
	app.Use(logger.New())
	app.Use(cors.New())
	// Read endpoints change only when a crawl writes, so clients that send
	// If-None-Match get cheap 304s between crawls.
	app.Use(etag.New())
	return app
}

func errorHandler(c *fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	if fe, ok := err.(*fiber.Error); ok {
		code = fe.Code
	}
	return c.Status(code).JSON(base.APIResponse{Message: err.Error()})
}

// ConnectDB opens the database selected by DB_DRIVER (postgres by default;
// sqlite is handy for tests and quick local runs) and migrates the schema.
func ConnectDB() *gorm.DB {
	cfg := &gorm.Config{Logger: gormlogger.Default.LogMode(logLevel())}

	var dial gorm.Dialector
	switch driver := base.Env("DB_DRIVER", "postgres"); driver {
	case "postgres":
		dsn := fmt.Sprintf("host=%s port=%s dbname=%s password=%s user=%s sslmode=%s TimeZone=UTC",
			base.Env("DB_HOST", "localhost"),
			base.Env("DB_PORT", "5432"),
			base.Env("DB_NAME", "howwebiz"),
			os.Getenv("DB_PASSWORD"),
			base.Env("DB_USER", "postgres"),
			base.Env("DB_SSLMODE", "disable"),
		)
		dial = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
	case "sqlite":
		dial = sqlite.Open(base.Env("DB_PATH", "howwebiz.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	default:
		log.Fatalf("unknown DB_DRIVER %q (postgres|sqlite)", driver)
	}

	db, err := connectWithRetry(dial, cfg)
	if err != nil {
		log.Fatalf("failed to start application due to db connection failure: %v", err)
	}
	if base.Env("DB_DRIVER", "postgres") == "sqlite" {
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(1)
	}

	if err := db.AutoMigrate(
		&song.Song{},
		&genre.Genre{},
		&chart.Snapshot{},
		&chart.Entry{},
		&crawler.CrawlRun{},
		&crawler.PageFingerprint{},
		&crawler.CatalogCursor{},
		&crawler.SongProbe{},
		&audiocom.Track{},
		&audiocom.Auth{},
		&audiocom.Feed{},
		&webguard.Health{},
		&momuziki.Song{},
		&momuziki.Category{},
		&momuziki.Cursor{},
		&momuziki.Probe{},
		&catalog.Song{},
		&catalog.Source{},
		&catalog.Watermark{},
		&sitecrawl.Cursor{},
		&sitecrawl.Probe{},
		&sitecrawl.Page{},
	); err != nil {
		log.Fatalf("failed to migrate application database: %v", err)
	}
	search.Setup(db)
	catalog.Setup(db)
	return db
}

// connectWithRetry waits for the database (e.g. a compose service that is
// still starting).
func connectWithRetry(dial gorm.Dialector, cfg *gorm.Config) (*gorm.DB, error) {
	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		db, err := gorm.Open(dial, cfg)
		if err == nil {
			sqlDB, _ := db.DB()
			if err = sqlDB.Ping(); err == nil {
				return db, nil
			}
		}
		lastErr = err
		slog.Warn("database not ready, retrying", "attempt", attempt, "err", err)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return nil, lastErr
}

func logLevel() gormlogger.LogLevel {
	switch base.Env("DB_LOG", "warn") {
	case "silent":
		return gormlogger.Silent
	case "error":
		return gormlogger.Error
	case "info":
		return gormlogger.Info
	}
	return gormlogger.Warn
}
