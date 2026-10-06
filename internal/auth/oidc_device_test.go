package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rmitchellscott/aviary/internal/database"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

func TestOIDCDeviceLoginBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("MULTI_USER", "true")
	t.Setenv("OIDC_DEVICE_LOGIN_ENABLED", "true")
	t.Setenv("OIDC_AUTO_CREATE_USERS", "false")
	t.Setenv("ALLOW_INSECURE", "true")

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var approved atomic.Bool
	var tokenCalls atomic.Int32
	var deviceStarts atomic.Int32
	const clientSecret = "device-client-secret"

	var provider *httptest.Server
	provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse device form: %v", err)
			}
			if r.Form.Get("client_id") != "device-client" || r.Form.Get("client_secret") != clientSecret {
				http.Error(w, "invalid_client", http.StatusUnauthorized)
				return
			}
			code := fmt.Sprintf("device-code-%d", deviceStarts.Add(1))
			writeJSON(t, w, map[string]any{
				"device_code":               code,
				"user_code":                 "ABCD-EFGH",
				"verification_uri":          providerURL(r) + "/verify",
				"verification_uri_complete": providerURL(r) + "/verify?device_code=" + url.QueryEscape(code),
				"expires_in":                60,
				"interval":                  1,
			})
		case "/token":
			tokenCalls.Add(1)
			if !validClient(r, clientSecret) {
				http.Error(w, "invalid_client", http.StatusUnauthorized)
				return
			}
			if !approved.Load() {
				writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "authorization_pending"})
				return
			}
			writeJSON(t, w, map[string]any{
				"access_token": "opaque-access-token",
				"token_type":   "Bearer",
				"expires_in":   3600,
				"id_token":     signedDeviceIDToken(t, privateKey, provider.URL),
			})
		case "/verify":
			approved.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	oldOIDCEnabled := oidcEnabled
	oldOAuth2Config := oauth2Config
	oldVerifier := oidcVerifier
	oldJWTSecret := jwtSecret
	oldDB := database.DB
	oldStore := pendingDeviceLogins
	testStore := newDeviceLoginStore()
	t.Cleanup(func() {
		testStore.stop()
		oidcEnabled = oldOIDCEnabled
		oauth2Config = oldOAuth2Config
		oidcVerifier = oldVerifier
		jwtSecret = oldJWTSecret
		database.DB = oldDB
		pendingDeviceLogins = oldStore
	})

	database.DB = testAuthDB(t)
	subject := "alex-device-subject"
	if err := database.DB.Create(&database.User{
		Username:    "alex",
		Email:       "alex@example.test",
		Password:    "test-password-hash",
		IsAdmin:     true,
		IsActive:    true,
		OidcSubject: &subject,
	}).Error; err != nil {
		t.Fatal(err)
	}
	jwtSecret = []byte("test-jwt-secret")
	oidcEnabled = true
	oauth2Config = &oauth2.Config{
		ClientID:     "device-client",
		ClientSecret: clientSecret,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: provider.URL + "/device",
			TokenURL:      provider.URL + "/token",
		},
		Scopes: []string{"openid", "profile", "email"},
	}
	oidcVerifier = oidc.NewVerifier(provider.URL, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{privateKey.Public()}}, &oidc.Config{ClientID: "device-client"})
	pendingDeviceLogins = testStore

	router := gin.New()
	router.POST("/api/auth/oidc/device/start", OIDCDeviceStartHandler)
	router.GET("/api/auth/oidc/device/status", OIDCDeviceStatusHandler)
	router.POST("/api/auth/oidc/device/finish", OIDCDeviceFinishHandler)
	router.POST("/api/auth/oidc/device/cancel", OIDCDeviceCancelHandler)
	router.GET("/api/auth/check", MultiUserCheckAuthHandler)
	app := httptest.NewServer(router)
	defer app.Close()
	client := clientWithCookies(t)

	forbidden := postJSON(t, client, app.URL+"/api/auth/oidc/device/start", "https://evil.example")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("cross-origin start status = %d, body = %s", forbidden.Code, forbidden.Body)
	}
	if deviceStarts.Load() != 0 {
		t.Fatalf("cross-origin start reached provider: %d calls", deviceStarts.Load())
	}

	start := postJSON(t, client, app.URL+"/api/auth/oidc/device/start", app.URL)
	if start.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", start.Code, start.Body)
	}
	var startData struct {
		Status    string `json:"status"`
		Link      string `json:"verification_uri_complete"`
		AttemptID string `json:"attempt_id"`
	}
	decodeJSON(t, start.Body, &startData)
	var startPayload map[string]json.RawMessage
	decodeJSON(t, start.Body, &startPayload)
	if startData.Status != string(deviceLoginPending) || startData.AttemptID == "" || !strings.HasPrefix(startData.Link, provider.URL+"/verify?") {
		t.Fatalf("unexpected device start response: %s", start.Body)
	}
	if _, exposed := startPayload["device_code"]; exposed {
		t.Fatalf("start response exposed the device code: %s", start.Body)
	}
	if deviceStarts.Load() != 1 {
		t.Fatalf("device endpoint calls = %d, want 1", deviceStarts.Load())
	}
	if !start.HasHttpOnlyProof {
		t.Fatal("device proof cookie was not HttpOnly")
	}
	if start.HasAuthCookie {
		t.Fatal("device start issued an auth cookie")
	}

	pending := getJSON(t, client, app.URL+"/api/auth/oidc/device/status?attempt_id="+url.QueryEscape(startData.AttemptID))
	if pending.Code != http.StatusOK || !strings.Contains(pending.Body, `"status":"pending"`) {
		t.Fatalf("pending status = %d, body = %s", pending.Code, pending.Body)
	}
	if pending.HasAuthCookie {
		t.Fatal("GET status issued an auth cookie")
	}
	callsBeforeApproval := tokenCalls.Load()
	if _, err := http.Get(startData.Link); err != nil {
		t.Fatal(err)
	}
	approvedStatus := waitForStatus(t, client, app.URL+"/api/auth/oidc/device/status?attempt_id="+url.QueryEscape(startData.AttemptID), `"status":"approved"`)
	if approvedStatus.HasAuthCookie {
		t.Fatal("approved GET status issued an auth cookie")
	}
	if tokenCalls.Load() <= callsBeforeApproval {
		t.Fatal("device worker did not poll the token endpoint")
	}

	finish := postDeviceJSON(t, client, app.URL+"/api/auth/oidc/device/finish", app.URL, startData.AttemptID)
	if finish.Code != http.StatusOK || !strings.Contains(finish.Body, `"authenticated":true`) {
		t.Fatalf("finish status = %d, body = %s", finish.Code, finish.Body)
	}
	if !finish.HasAuthCookie {
		t.Fatal("finish did not issue an auth cookie")
	}
	if finish.HasHttpOnlyProof {
		t.Fatal("finish mutated the proof cookie after authentication")
	}
	authCheck := getJSON(t, client, app.URL+"/api/auth/check")
	if authCheck.Code != http.StatusOK || !strings.Contains(authCheck.Body, `"authenticated":true`) {
		t.Fatalf("auth check status = %d, body = %s", authCheck.Code, authCheck.Body)
	}
	if deviceStarts.Load() != 1 {
		t.Fatalf("unexpected device start calls after finish: %d", deviceStarts.Load())
	}

	approved.Store(false)
	secondStart := postJSON(t, client, app.URL+"/api/auth/oidc/device/start", app.URL)
	if secondStart.Code != http.StatusOK {
		t.Fatalf("second start status = %d, body = %s", secondStart.Code, secondStart.Body)
	}
	var secondStartData struct {
		AttemptID string `json:"attempt_id"`
		Link      string `json:"verification_uri_complete"`
	}
	decodeJSON(t, secondStart.Body, &secondStartData)
	if secondStartData.AttemptID == "" || secondStartData.Link == "" {
		t.Fatalf("second start omitted attempt binding: %s", secondStart.Body)
	}
	staleStatus := getJSON(t, client, app.URL+"/api/auth/oidc/device/status?attempt_id="+url.QueryEscape(startData.AttemptID))
	if staleStatus.Code != http.StatusOK || !strings.Contains(staleStatus.Body, `"status":"expired"`) || staleStatus.HasHttpOnlyProof {
		t.Fatalf("stale status response = %d, body = %s, proof cookie = %t", staleStatus.Code, staleStatus.Body, staleStatus.HasHttpOnlyProof)
	}
	staleFinish := postDeviceJSON(t, client, app.URL+"/api/auth/oidc/device/finish", app.URL, startData.AttemptID)
	if staleFinish.Code != http.StatusGone || staleFinish.HasHttpOnlyProof {
		t.Fatalf("stale finish response = %d, body = %s, proof cookie = %t", staleFinish.Code, staleFinish.Body, staleFinish.HasHttpOnlyProof)
	}
	staleCancel := postDeviceJSON(t, client, app.URL+"/api/auth/oidc/device/cancel", app.URL, startData.AttemptID)
	if staleCancel.Code != http.StatusOK || staleCancel.HasHttpOnlyProof {
		t.Fatalf("stale cancel response = %d, body = %s, proof cookie = %t", staleCancel.Code, staleCancel.Body, staleCancel.HasHttpOnlyProof)
	}
	secondPending := getJSON(t, client, app.URL+"/api/auth/oidc/device/status?attempt_id="+url.QueryEscape(secondStartData.AttemptID))
	if secondPending.Code != http.StatusOK || !strings.Contains(secondPending.Body, `"status":"pending"`) {
		t.Fatalf("replacement attempt was changed by stale cancel: %d, body = %s", secondPending.Code, secondPending.Body)
	}
	if _, err := http.Get(secondStartData.Link); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, client, app.URL+"/api/auth/oidc/device/status?attempt_id="+url.QueryEscape(secondStartData.AttemptID), `"status":"approved"`)
	firstFinishClient := cloneClientWithCookies(t, client, app.URL)
	secondFinishClient := cloneClientWithCookies(t, client, app.URL)
	results := make(chan boundaryResponse, 2)
	go func() {
		results <- postDeviceJSON(t, firstFinishClient, app.URL+"/api/auth/oidc/device/finish", app.URL, secondStartData.AttemptID)
	}()
	go func() {
		results <- postDeviceJSON(t, secondFinishClient, app.URL+"/api/auth/oidc/device/finish", app.URL, secondStartData.AttemptID)
	}()
	firstResult, secondResult := <-results, <-results
	if !((firstResult.Code == http.StatusOK && secondResult.Code == http.StatusGone) || (firstResult.Code == http.StatusGone && secondResult.Code == http.StatusOK)) {
		t.Fatalf("concurrent finish statuses = %d and %d, want one 200 and one 410", firstResult.Code, secondResult.Code)
	}
	if (firstResult.Code == http.StatusGone && firstResult.HasHttpOnlyProof) || (secondResult.Code == http.StatusGone && secondResult.HasHttpOnlyProof) {
		t.Fatal("late concurrent finish returned a proof-cookie mutation")
	}
	if deviceStarts.Load() != 2 {
		t.Fatalf("device endpoint calls = %d, want 2", deviceStarts.Load())
	}
}

func TestOIDCCallbackRejectsEmptyNonce(t *testing.T) {
	oldOIDCEnabled := oidcEnabled
	t.Cleanup(func() { oidcEnabled = oldOIDCEnabled })
	oidcEnabled = true

	router := gin.New()
	router.GET("/callback", OIDCCallbackHandler)
	request := httptest.NewRequest(http.MethodGet, "/callback?state=expected-state&code=unused", nil)
	request.AddCookie(&http.Cookie{Name: "oidc_state", Value: "expected-state"})
	request.AddCookie(&http.Cookie{Name: "oidc_nonce", Value: ""})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("empty browser nonce status = %d, body = %s", response.Code, response.Body.String())
	}
}

type boundaryResponse struct {
	Code             int
	Body             string
	HasAuthCookie    bool
	HasHttpOnlyProof bool
}

func clientWithCookies(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func postJSON(t *testing.T, client *http.Client, endpoint, origin string) boundaryResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return responseDetails(t, resp)
}

func postDeviceJSON(t *testing.T, client *http.Client, endpoint, origin, attemptID string) boundaryResponse {
	t.Helper()
	body, err := json.Marshal(map[string]string{"attempt_id": attemptID})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return responseDetails(t, resp)
}

func cloneClientWithCookies(t *testing.T, source *http.Client, endpoint string) *http.Client {
	t.Helper()
	clone := clientWithCookies(t)
	parsed, err := url.Parse(endpoint + "/api/auth/oidc/device")
	if err != nil {
		t.Fatal(err)
	}
	clone.Jar.SetCookies(parsed, source.Jar.Cookies(parsed))
	return clone
}

func getJSON(t *testing.T, client *http.Client, endpoint string) boundaryResponse {
	t.Helper()
	resp, err := client.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return responseDetails(t, resp)
}

func waitForStatus(t *testing.T, client *http.Client, endpoint, expected string) boundaryResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response := getJSON(t, client, endpoint)
		if strings.Contains(response.Body, expected) {
			return response
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("status never reached %q", expected)
	return boundaryResponse{}
}

func responseDetails(t *testing.T, response *http.Response) boundaryResponse {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	details := boundaryResponse{Code: response.StatusCode, Body: string(body)}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "auth_token" {
			details.HasAuthCookie = true
		}
		if cookie.Name == deviceLoginProofCookie {
			details.HasHttpOnlyProof = cookie.HttpOnly && cookie.SameSite == http.SameSiteStrictMode
		}
	}
	return details
}

func providerURL(r *http.Request) string {
	return "http://" + r.Host
}

func validClient(r *http.Request, expectedSecret string) bool {
	if _, secret, ok := r.BasicAuth(); ok && secret == expectedSecret {
		return true
	}
	return r.FormValue("client_secret") == expectedSecret
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func signedDeviceIDToken(t *testing.T, privateKey *rsa.PrivateKey, issuer string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":                issuer,
		"sub":                "alex-device-subject",
		"aud":                "device-client",
		"exp":                time.Now().Add(time.Hour).Unix(),
		"iat":                time.Now().Unix(),
		"preferred_username": "alex",
		"email":              "alex@example.test",
		"name":               "Alex",
		"email_verified":     true,
		"groups":             []string{"readers"},
	})
	raw, err := token.SignedString(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testAuthDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.User{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func decodeJSON(t *testing.T, body string, target any) {
	t.Helper()
	if err := json.NewDecoder(strings.NewReader(body)).Decode(target); err != nil {
		t.Fatalf("decode JSON %q: %v", body, err)
	}
}
