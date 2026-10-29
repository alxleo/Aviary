package database

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestCreateUserReadingDefaults(t *testing.T) {
	for _, test := range []struct {
		name, folder, format, wantFolder, wantFormat string
	}{
		{"unconfigured", "", "", "/", "epub"},
		{"configured", "Reading Inbox", "pdf", "Reading Inbox", "pdf"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("RM_TARGET_DIR", test.folder)
			t.Setenv("RM_TARGET_DIR_FILE", "")
			t.Setenv("CONVERSION_OUTPUT_FORMAT", test.format)
			t.Setenv("CONVERSION_OUTPUT_FORMAT_FILE", "")
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "users.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&User{}); err != nil {
				t.Fatal(err)
			}
			user, err := NewUserService(db).CreateUser("reader", "reader@example.com", "test-password", false)
			if err != nil {
				t.Fatal(err)
			}
			var saved User
			if err := db.First(&saved, "id = ?", user.ID).Error; err != nil {
				t.Fatal(err)
			}
			if saved.DefaultRmdir != test.wantFolder || saved.ConversionOutputFormat != test.wantFormat {
				t.Fatalf("saved reading defaults = %q, %q; want %q, %q", saved.DefaultRmdir, saved.ConversionOutputFormat, test.wantFolder, test.wantFormat)
			}
		})
	}
}
