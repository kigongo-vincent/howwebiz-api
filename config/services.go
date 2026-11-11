package config

import (
	"log"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		// Without these, slow or idle clients hold connections (and their
		// buffers) forever. Writes allow for the live provider search.
		ReadTimeout:  base.EnvDuration("HTTP_READ_TIMEOUT", 30*time.Second),
		WriteTimeout: base.EnvDuration("HTTP_WRITE_TIMEOUT", 60*time.Second),
		IdleTimeout:  base.EnvDuration("HTTP_IDLE_TIMEOUT", 120*time.Second),
	})
	app.Use(recover.New(recover.Config{EnableStackTrace: true}))
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
		// say plainly which settings didn't arrive (a container started without
		// its env file otherwise shows up as a baffling "user=root" on localhost)
		var missing []string
		for _, k := range []string{"DB_HOST", "DB_NAME", "DB_USER", "DB_PASSWORD"} {
			if strings.TrimSpace(os.Getenv(k)) == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			slog.Warn("database settings not set, using defaults: check the env file this process was started with", "missing", strings.Join(missing, ","))
		}
		// a URL, with every part escaped: an empty or odd password can't shift
		// the other settings (in "key=value" form an empty password swallowed user=)
		q := url.Values{"sslmode": {base.Env("DB_SSLMODE", "disable")}, "TimeZone": {"UTC"}}
		dsn := (&url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(base.Env("DB_USER", "postgres"), os.Getenv("DB_PASSWORD")),
			Host:     net.JoinHostPort(base.Env("DB_HOST", "localhost"), base.Env("DB_PORT", "5432")),
			Path:     "/" + base.Env("DB_NAME", "howwebiz"),
			RawQuery: q.Encode(),
		}).String()
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
	if sqlDB, err := db.DB(); err == nil {
		if base.Env("DB_DRIVER", "postgres") == "sqlite" {
			sqlDB.SetMaxOpenConns(1)
		} else {
			// a ceiling on connections: crawlers, ingest and the API together
			// otherwise open as many as they like, and a small database (shared
			// with other apps) spends its time switching between them. Past the
			// ceiling, queries wait their turn in the app instead.
			sqlDB.SetMaxOpenConns(base.EnvInt("DB_MAX_CONNS", 15))
			sqlDB.SetMaxIdleConns(5)
			sqlDB.SetConnMaxIdleTime(5 * time.Minute)
		}
	}

	if err := migrate(db,
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
		&sitecrawl.FeedCursor{},
	); err != nil {
		log.Fatalf("failed to migrate application database: %v", err)
	}
	// Only the fast part here; indexes are built in the background once the
	// API is listening (see Wire).
	search.Setup(db)
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
