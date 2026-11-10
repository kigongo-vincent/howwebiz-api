package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/kigongo-vincent/howwebiz-api/modules/base"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// schemaVersion records the fingerprint of the models last migrated.
type schemaVersion struct {
	ID        uint   `gorm:"primaryKey"`
	Hash      string `gorm:"size:64;not null"`
	UpdatedAt time.Time
}

func (schemaVersion) TableName() string { return "schema_version" }

// migrate runs AutoMigrate only when the models changed since the last
// successful run. AutoMigrate inspects every table and column through
// information_schema; on a busy or small database that alone took ~40s per
// boot, and the API can't serve until it is done. DB_AUTOMIGRATE=always
// forces it.
func migrate(db *gorm.DB, models ...any) error {
	if err := db.AutoMigrate(&schemaVersion{}); err != nil {
		return err
	}
	hash, err := fingerprint(db, models)
	if err != nil {
		return err
	}
	var cur schemaVersion
	db.Limit(1).Find(&cur, 1)
	if cur.Hash == hash && base.Env("DB_AUTOMIGRATE", "auto") != "always" {
		slog.Info("schema unchanged, migration skipped", "fingerprint", hash[:12])
		return nil
	}
	start := time.Now()
	if err := db.AutoMigrate(models...); err != nil {
		return err
	}
	slog.Info("schema migrated", "took", time.Since(start).Round(time.Millisecond), "fingerprint", hash[:12])
	return db.Save(&schemaVersion{ID: 1, Hash: hash, UpdatedAt: time.Now().UTC()}).Error
}

// fingerprint hashes everything AutoMigrate acts on: each model's table,
// columns, data types and gorm tags (size, index, default, not null, ...).
func fingerprint(db *gorm.DB, models []any) (string, error) {
	h := sha256.New()
	cache := &sync.Map{}
	for _, m := range models {
		s, err := schema.Parse(m, cache, db.NamingStrategy)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "table %s\n", s.Table)
		for _, f := range s.Fields {
			if f.DBName == "" {
				continue // relations, ignored fields
			}
			fmt.Fprintf(h, "  %s %s %s", f.DBName, f.DataType, f.FieldType)
			for _, k := range slices.Sorted(maps.Keys(f.TagSettings)) {
				fmt.Fprintf(h, " %s=%s", k, f.TagSettings[k])
			}
			fmt.Fprintln(h)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
