package database

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestCreateUserReadingDefaults(t *testing.T) {
	for _, test := range []struct {
		name, folder, format, wantFolder, wantFormat string
		resolution, dpi, cover, exclusions           string
		depth                                        int
	}{
		{name: "unconfigured", wantFolder: "/", wantFormat: "epub", cover: "current"},
		{name: "configured", folder: "Reading Inbox", format: "pdf", wantFolder: "Reading Inbox", wantFormat: "pdf", resolution: "1620x2160", dpi: "229", cover: "first", exclusions: "Templates", depth: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("RM_TARGET_DIR", test.folder)
			t.Setenv("RM_TARGET_DIR_FILE", "")
			t.Setenv("CONVERSION_OUTPUT_FORMAT", test.format)
			t.Setenv("CONVERSION_OUTPUT_FORMAT_FILE", "")
			t.Setenv("PAGE_RESOLUTION", test.resolution)
			t.Setenv("PAGE_DPI", test.dpi)
			t.Setenv("RMAPI_COVERPAGE", test.cover)
			t.Setenv("RMAPI_FOLDER_EXCLUSION_LIST", test.exclusions)
			t.Setenv("RMAPI_FOLDER_DEPTH_LIMIT", fmt.Sprint(test.depth))
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
			if saved.PageResolution != test.resolution || saved.CoverpageSetting != test.cover || saved.FolderDepthLimit != test.depth || saved.FolderExclusionList != test.exclusions {
				t.Fatalf("unexpected saved reading preferences: resolution=%q cover=%q depth=%d exclusions=%q", saved.PageResolution, saved.CoverpageSetting, saved.FolderDepthLimit, saved.FolderExclusionList)
			}
			wantDPI := 0.0
			if test.name == "configured" {
				wantDPI = 229
			}
			if saved.PageDPI != wantDPI {
				t.Fatalf("saved DPI = %v; want %v", saved.PageDPI, wantDPI)
			}
		})
	}
}
