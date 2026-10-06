package rmapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func rmapiRequestContext(t *testing.T, user interface{}, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Set("user", user)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/profile/pair", nil)
	handler(context)
	return recorder
}

func TestSharedBorrowerCannotPairOrUnpair(t *testing.T) {
	t.Setenv("MULTI_USER", "true")
	t.Setenv("RMAPI_SHARED_USER", "tablet-owner")
	t.Setenv("DRY_RUN", "true")
	db := rmapiTestDB(t)
	owner, borrower := rmapiTestUsers(t, db, "owner-config")

	for name, handler := range map[string]gin.HandlerFunc{
		"pair":   PairHandler,
		"unpair": UnpairHandler,
	} {
		t.Run(name, func(t *testing.T) {
			response := rmapiRequestContext(t, &borrower, handler)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusForbidden, response.Body.String())
			}
		})
	}

	response := rmapiRequestContext(t, &owner, PairHandler)
	if response.Code != http.StatusOK {
		t.Fatalf("owner pair status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
}
