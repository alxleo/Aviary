package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/rmitchellscott/aviary/internal/database"
	"gorm.io/gorm"
)

func authSharedTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:auth-shared-test-%s?mode=memory&cache=shared", uuid.NewString())), &gorm.Config{})
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

func authSharedTestUsers(t *testing.T, db *gorm.DB) (database.User, database.User) {
	t.Helper()
	owner := database.User{
		ID:          uuid.New(),
		Username:    "tablet-owner",
		Email:       "tablet-owner@example.test",
		Password:    "password",
		IsActive:    true,
		IsAdmin:     true,
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
	return owner, borrower
}

func authSharedRequestContext(t *testing.T, method, path string, user *database.User, payload any) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	context.Request = httptest.NewRequest(method, path, bytes.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")
	context.Set("user", user)
	return context, recorder
}

func TestSharedBorrowerCannotMutateRmapiHost(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	db := authSharedTestDB(t)
	owner, borrower := authSharedTestUsers(t, db)
	t.Setenv("RMAPI_SHARED_USER_ID", owner.ID.String())

	t.Run("self", func(t *testing.T) {
		context, recorder := authSharedRequestContext(t, http.MethodPut, "/api/profile", &borrower, map[string]string{
			"rmapi_host": "https://borrower.example.test",
		})
		UpdateCurrentUserHandler(context)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
		}
	})

	t.Run("admin editing borrower", func(t *testing.T) {
		context, recorder := authSharedRequestContext(t, http.MethodPut, "/api/users/"+borrower.ID.String(), &owner, map[string]string{
			"rmapi_host": "https://admin.example.test",
		})
		context.Params = gin.Params{{Key: "id", Value: borrower.ID.String()}}
		UpdateUserHandler(context)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
		}
	})

	var stored database.User
	if err := db.First(&stored, "id = ?", borrower.ID).Error; err != nil {
		t.Fatalf("load borrower: %v", err)
	}
	if stored.RmapiHost != "" {
		t.Fatalf("borrower host changed to %q after guarded updates", stored.RmapiHost)
	}

	response := userToResponse(&borrower)
	if !response.RmapiShared || !response.RmapiPaired || response.RmapiHost != owner.RmapiHost {
		t.Fatalf("auth response destination = shared:%t paired:%t host:%q", response.RmapiShared, response.RmapiPaired, response.RmapiHost)
	}
}
