package base

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"
)

var createIndexRe = regexp.MustCompile(`(?i)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+IF\s+NOT\s+EXISTS\s+(\w+)`)

// EnsureIndexes runs idempotent index statements (CREATE INDEX IF NOT
// EXISTS / DROP INDEX IF EXISTS / CREATE EXTENSION). On Postgres indexes are
// built CONCURRENTLY: the table stays writable and the API keeps serving
// while a large index builds, which on a slow database can take minutes.
// An index left INVALID by an interrupted concurrent build (a deploy
// stopped the process mid-build) is dropped and built again.
//
// Best effort: a failed statement is logged and skipped; a missing index
// makes queries slower, never wrong. Stops early when ctx is cancelled.
func EnsureIndexes(ctx context.Context, db *gorm.DB, stmts []string) {
	pg := db.Dialector.Name() == "postgres"
	for _, stmt := range stmts {
		if ctx.Err() != nil {
			return
		}
		if pg {
			if m := createIndexRe.FindStringSubmatch(stmt); m != nil {
				dropInvalidIndex(db, m[2])
				stmt = createIndexRe.ReplaceAllString(stmt, "CREATE ${1}INDEX CONCURRENTLY IF NOT EXISTS $2")
			} else if rest, ok := strings.CutPrefix(stmt, "DROP INDEX IF EXISTS "); ok {
				stmt = "DROP INDEX CONCURRENTLY IF EXISTS " + rest
			}
		}
		start := time.Now()
		if err := db.WithContext(ctx).Exec(stmt).Error; err != nil {
			slog.Warn("index setup skipped", "sql", stmt, "err", err)
			continue
		}
		if took := time.Since(start); took > time.Second {
			slog.Info("index ready", "sql", stmt, "took", took.Round(time.Millisecond))
		}
	}
}

func dropInvalidIndex(db *gorm.DB, name string) {
	var invalid int64
	db.Raw(`SELECT COUNT(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = ? AND NOT i.indisvalid`, name).Scan(&invalid)
	if invalid > 0 {
		slog.Warn("dropping index left invalid by an interrupted build", "index", name)
		if err := db.Exec(`DROP INDEX CONCURRENTLY IF EXISTS ` + name).Error; err != nil {
			slog.Warn("drop invalid index", "index", name, "err", err)
		}
	}
}
