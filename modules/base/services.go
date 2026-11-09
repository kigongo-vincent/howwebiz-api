package base

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
)

func API_ERROR(c *fiber.Ctx, v string) error {
	return c.Status(fiber.StatusBadRequest).JSON(APIResponse{Message: v})
}

// API_FAIL responds with an explicit status code.
func API_FAIL(c *fiber.Ctx, status int, v string) error {
	return c.Status(status).JSON(APIResponse{Message: v})
}

func ParamID(c *fiber.Ctx, name string) (uint, bool) {
	id, err := strconv.ParseUint(c.Params(name), 10, 64)
	return uint(id), err == nil && id > 0
}

// ---- env helpers shared by modules ----

func Env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func EnvInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return def
}

func EnvBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return def
}

func EnvDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return def
}

func EnvList(key string, def []string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	var out []string
	for p := range strings.SplitSeq(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var (
	locOnce sync.Once
	loc     *time.Location
)

// Location is the app timezone (CRON_TZ, default Africa/Kampala). It
// defines when the daily crawl fires and how ?date= is interpreted.
func Location() *time.Location {
	locOnce.Do(func() {
		var err error
		if loc, err = time.LoadLocation(Env("CRON_TZ", "Africa/Kampala")); err != nil {
			log.Printf("invalid CRON_TZ, using UTC: %v", err)
			loc = time.UTC
		}
	})
	return loc
}
