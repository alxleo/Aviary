package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rmitchellscott/aviary/internal/database"
)

func TestAdminUpdatesConversionPreference(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	gin.SetMode(gin.TestMode)
	db := authSharedTestDB(t)
	owner, reader := authSharedTestUsers(t, db)

	for _, test := range []struct {
		name   string
		actor  database.User
		status int
		format string
	}{
		{"reader cannot use admin update route", reader, http.StatusForbidden, "epub"},
		{"admin can update format alone", owner, http.StatusOK, "pdf"},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.PUT("/api/users/:id", func(c *gin.Context) {
				c.Set("user", &test.actor)
				UpdateUserHandler(c)
			})
			request := httptest.NewRequest(http.MethodPut, "/api/users/"+reader.ID.String(), bytes.NewBufferString(`{"conversion_output_format":"pdf"}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("update format: %d %s", response.Code, response.Body)
			}
			var saved database.User
			if err := db.First(&saved, "id = ?", reader.ID).Error; err != nil {
				t.Fatal(err)
			}
			if saved.ConversionOutputFormat != test.format || saved.DefaultRmdir != reader.DefaultRmdir {
				t.Fatalf("reading preferences = %q, %q; want %q, %q", saved.ConversionOutputFormat, saved.DefaultRmdir, test.format, reader.DefaultRmdir)
			}
		})
	}
}

func TestAdminUpdatesFolderFilters(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	gin.SetMode(gin.TestMode)
	db := authSharedTestDB(t)
	owner, reader := authSharedTestUsers(t, db)
	router := gin.New()
	router.PUT("/api/users/:id", func(c *gin.Context) {
		c.Set("user", &owner)
		UpdateUserHandler(c)
	})
	for _, body := range []string{`{"folder_depth_limit":2,"folder_exclusion_list":"Templates"}`, `{"folder_depth_limit":0,"folder_exclusion_list":""}`} {
		request := httptest.NewRequest(http.MethodPut, "/api/users/"+reader.ID.String(), bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("update folder filter: %d %s", response.Code, response.Body)
		}
		var saved database.User
		if err := db.First(&saved, "id = ?", reader.ID).Error; err != nil {
			t.Fatal(err)
		}
		var expected struct {
			Depth      int    `json:"folder_depth_limit"`
			Exclusions string `json:"folder_exclusion_list"`
		}
		if err := json.Unmarshal([]byte(body), &expected); err != nil {
			t.Fatal(err)
		}
		if saved.FolderDepthLimit != expected.Depth || saved.FolderExclusionList != expected.Exclusions {
			t.Fatalf("saved folder filters = %d, %q; want %d, %q", saved.FolderDepthLimit, saved.FolderExclusionList, expected.Depth, expected.Exclusions)
		}
	}
}
