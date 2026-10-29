package auth

import (
	"bytes"
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
