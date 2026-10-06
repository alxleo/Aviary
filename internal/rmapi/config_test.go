package rmapi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/rmitchellscott/aviary/internal/database"
	"gorm.io/gorm"
)

func rmapiTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:rmapi-test-%s?mode=memory&cache=shared", uuid.NewString())), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if err := db.AutoMigrate(&database.User{}); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	previous := database.DB
	database.DB = db
	t.Cleanup(func() {
		database.DB = previous
	})
	return db
}

func rmapiTestUsers(t *testing.T, db *gorm.DB, ownerConfig string) (database.User, database.User) {
	t.Helper()

	owner := database.User{
		ID:          uuid.New(),
		Username:    "tablet-owner",
		Email:       "tablet-owner@example.test",
		Password:    "password",
		IsActive:    true,
		RmapiHost:   "https://tablet.example.test",
		RmapiConfig: ownerConfig,
	}
	borrower := database.User{
		ID:        uuid.New(),
		Username:  "family-member",
		Email:     "family-member@example.test",
		Password:  "password",
		IsActive:  true,
		RmapiHost: "https://borrower.example.test",
	}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := db.Create(&borrower).Error; err != nil {
		t.Fatalf("create borrower: %v", err)
	}
	return owner, borrower
}

func commandEnv(cmd *exec.Cmd) map[string]string {
	values := make(map[string]string)
	for _, item := range cmd.Env {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) == 2 {
			values[parts[0]] = parts[1]
		}
	}
	return values
}

func TestNewCommandUsesSharedDestinationAndCallerCache(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("RMAPI_SHARED_USER", "tablet-owner")
	db := rmapiTestDB(t)
	owner, borrower := rmapiTestUsers(t, db, "owner-config")

	previousCommand := ExecCommand
	ExecCommand = func(name string, args ...string) *exec.Cmd {
		return exec.Command("true")
	}
	t.Cleanup(func() { ExecCommand = previousCommand })

	cmd, cleanup, err := NewCommand(&borrower, "put", "file.pdf", "/")
	if err != nil {
		t.Fatalf("NewCommand returned error: %v", err)
	}
	defer cleanup()

	env := commandEnv(cmd)
	if env["RMAPI_HOST"] != owner.RmapiHost {
		t.Fatalf("RMAPI_HOST = %q, want %q", env["RMAPI_HOST"], owner.RmapiHost)
	}
	if env["XDG_CACHE_HOME"] != GetUserCachePath(borrower.ID) {
		t.Fatalf("XDG_CACHE_HOME = %q, want caller cache %q", env["XDG_CACHE_HOME"], GetUserCachePath(borrower.ID))
	}

	configPath := env["RMAPI_CONFIG"]
	if !strings.HasPrefix(filepath.Base(configPath), tempConfigPrefix) {
		t.Fatalf("config path %q does not use unique temp prefix", configPath)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat temp config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("temp config mode = %04o, want 0600", got)
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read temp config: %v", err)
	}
	if string(content) != owner.RmapiConfig {
		t.Fatalf("temp config = %q, want owner config %q", content, owner.RmapiConfig)
	}

	cleanup()
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("temp config still exists after cleanup, stat error: %v", err)
	}

	if !IsUserPaired(borrower.ID) {
		t.Fatal("shared borrower should be paired from the active owner's config")
	}
	status := DestinationStatusForUser(&borrower)
	if !status.Shared || !status.Paired || status.Host != owner.RmapiHost {
		t.Fatalf("borrower destination status = %+v", status)
	}
	ownerStatus := DestinationStatusForUser(&owner)
	if ownerStatus.Shared || !ownerStatus.Paired || ownerStatus.Host != owner.RmapiHost {
		t.Fatalf("owner destination status = %+v", ownerStatus)
	}
}

func TestNewCommandDefaultDestinationUsesCallerConfig(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("RMAPI_SHARED_USER", "")
	db := rmapiTestDB(t)
	_, borrower := rmapiTestUsers(t, db, "owner-config")
	borrower.RmapiConfig = "borrower-config"
	if err := db.Save(&borrower).Error; err != nil {
		t.Fatalf("save borrower: %v", err)
	}

	previousCommand := ExecCommand
	ExecCommand = func(name string, args ...string) *exec.Cmd {
		return exec.Command("true")
	}
	t.Cleanup(func() { ExecCommand = previousCommand })

	cmd, cleanup, err := NewCommand(&borrower, "ls", "/")
	if err != nil {
		t.Fatalf("NewCommand returned error: %v", err)
	}
	defer cleanup()

	env := commandEnv(cmd)
	if env["RMAPI_HOST"] != borrower.RmapiHost {
		t.Fatalf("RMAPI_HOST = %q, want caller host %q", env["RMAPI_HOST"], borrower.RmapiHost)
	}
	content, err := os.ReadFile(env["RMAPI_CONFIG"])
	if err != nil {
		t.Fatalf("read caller config: %v", err)
	}
	if string(content) != borrower.RmapiConfig {
		t.Fatalf("config = %q, want caller config %q", content, borrower.RmapiConfig)
	}
	if DestinationStatusForUser(&borrower).Shared {
		t.Fatal("default destination should not be marked shared")
	}
}

func TestNewCommandRequiresCallerInMultiUserMode(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("RMAPI_SHARED_USER", "")

	cmd, cleanup, err := NewCommand(nil, "ls", "/")
	cleanup()
	if err == nil {
		t.Fatal("NewCommand succeeded without an authenticated multi-user caller")
	}
	if cmd != nil {
		t.Fatal("NewCommand returned a command without an authenticated caller")
	}
}

func TestNewCommandFailsClosedForSharedOwner(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("RMAPI_SHARED_USER", "tablet-owner")

	tests := []struct {
		name      string
		setup     func(*gorm.DB, database.User)
		wantInErr string
	}{
		{
			name: "owner missing",
			setup: func(db *gorm.DB, owner database.User) {
				db.Delete(&owner)
			},
			wantInErr: "shared rmapi owner",
		},
		{
			name: "owner inactive",
			setup: func(db *gorm.DB, owner database.User) {
				db.Model(&database.User{}).Where("id = ?", owner.ID).Update("is_active", false)
			},
			wantInErr: "inactive",
		},
		{
			name: "owner unpaired",
			setup: func(db *gorm.DB, owner database.User) {
				db.Model(&database.User{}).Where("id = ?", owner.ID).Update("rmapi_config", "")
			},
			wantInErr: "has no rmapi config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := rmapiTestDB(t)
			owner, borrower := rmapiTestUsers(t, db, "owner-config")
			if tt.setup != nil {
				tt.setup(db, owner)
			}

			cmd, cleanup, err := NewCommand(&borrower, "put")
			cleanup()
			if err == nil {
				t.Fatal("NewCommand succeeded for unavailable shared owner")
			}
			if cmd != nil {
				t.Fatal("NewCommand returned a runnable command on resolution failure")
			}
			if !strings.Contains(err.Error(), tt.wantInErr) {
				t.Fatalf("error = %q, want substring %q", err, tt.wantInErr)
			}
			if IsUserPaired(borrower.ID) {
				t.Fatal("borrower should be unpaired when shared owner is unavailable")
			}
		})
	}
}
