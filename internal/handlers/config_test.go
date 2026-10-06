package handlers

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/rmitchellscott/aviary/internal/database"
	"gorm.io/gorm"
)

func configTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:config-test-%s?mode=memory&cache=shared", uuid.NewString())), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if err := db.AutoMigrate(&database.User{}); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	previous := database.DB
	database.DB = db
	t.Cleanup(func() { database.DB = previous })
	return db
}

func TestConfigHandlerReportsEffectiveSharedDestination(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("RMAPI_SHARED_USER", "tablet-owner")
	db := configTestDB(t)
	owner := database.User{
		ID:          uuid.New(),
		Username:    "tablet-owner",
		Email:       "tablet-owner@example.test",
		Password:    "password",
		IsActive:    true,
		RmapiHost:   "https://tablet.example.test",
		RmapiConfig: "owner-config",
	}
	borrower := database.User{
		ID:       uuid.New(),
		Username: "family-member",
		Email:    "family-member@example.test",
		Password: "password",
		IsActive: true,
	}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := db.Create(&borrower).Error; err != nil {
		t.Fatalf("create borrower: %v", err)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Set("user", &borrower)
	ConfigHandler(context)
	if recorder.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["rmapi_shared"] != true {
		t.Fatalf("rmapi_shared = %v, want true", response["rmapi_shared"])
	}
	if response["rmapi_host"] != owner.RmapiHost {
		t.Fatalf("rmapi_host = %v, want %q", response["rmapi_host"], owner.RmapiHost)
	}
	if response["rmapi_paired"] != true {
		t.Fatalf("rmapi_paired = %v, want true", response["rmapi_paired"])
	}
	if _, exists := response["rmapi_shared_user"]; exists {
		t.Fatal("config response exposed shared owner identity")
	}
}
