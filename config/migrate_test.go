package config

import (
	"fmt"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fpV1 struct {
	ID   uint
	Name string `gorm:"size:16"`
}

func (fpV1) TableName() string { return "fp_things" }

type fpV2 struct {
	ID   uint
	Name string `gorm:"size:64"` // widened
}

func (fpV2) TableName() string { return "fp_things" }

type fpV3 struct {
	ID    uint
	Name  string `gorm:"size:64"`
	Extra string // new column
}

func (fpV3) TableName() string { return "fp_things" }

func TestMigrateSkipsUnchangedSchema(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	hash := func(m any) string {
		h, err := fingerprint(db, []any{m})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if hash(&fpV1{}) == hash(&fpV2{}) || hash(&fpV2{}) == hash(&fpV3{}) {
		t.Fatal("a tag or column change must change the fingerprint")
	}
	if hash(&fpV1{}) != hash(&fpV1{}) {
		t.Fatal("fingerprint is not stable")
	}

	if err := migrate(db, &fpV1{}); err != nil {
		t.Fatal(err)
	}
	// Unchanged models: AutoMigrate is skipped, so a column dropped behind
	// its back stays dropped...
	if err := db.Migrator().DropColumn(&fpV1{}, "name"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, &fpV1{}); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasColumn(&fpV1{}, "name") {
		t.Fatal("migration ran although the schema was unchanged")
	}
	// ...and a model change migrates again.
	if err := migrate(db, &fpV3{}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&fpV3{}, "extra") || !db.Migrator().HasColumn(&fpV3{}, "name") {
		t.Fatal("changed schema was not migrated")
	}
	// DB_AUTOMIGRATE=always forces it
	if err := db.Migrator().DropColumn(&fpV3{}, "extra"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_AUTOMIGRATE", "always")
	if err := migrate(db, &fpV3{}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&fpV3{}, "extra") {
		t.Fatal("DB_AUTOMIGRATE=always did not migrate")
	}
}
