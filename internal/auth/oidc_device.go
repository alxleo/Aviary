package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rmitchellscott/aviary/internal/config"
	"github.com/rmitchellscott/aviary/internal/database"
	"golang.org/x/oauth2"
)

const (
	deviceLoginProofCookie = "oidc_device_proof"
	deviceLoginTTL         = 10 * time.Minute
	maxPendingDeviceLogins = 32
)

type deviceLoginStatus string

const (
	deviceLoginPending  deviceLoginStatus = "pending"
	deviceLoginApproved deviceLoginStatus = "approved"
	deviceLoginFailed   deviceLoginStatus = "failed"
	deviceLoginExpired  deviceLoginStatus = "expired"
)

type deviceLoginTransaction struct {
	proof     string
	device    oauth2.DeviceAuthResponse
	expiresAt time.Time
	cancel    context.CancelFunc

	status deviceLoginStatus
	token  *oauth2.Token
}

type deviceLoginStore struct {
	mu           sync.Mutex
	transactions map[string]*deviceLoginTransaction
	reservations int
	janitorOnce  sync.Once
}

var pendingDeviceLogins = newDeviceLoginStore()

func newDeviceLoginStore() *deviceLoginStore {
	return &deviceLoginStore{transactions: make(map[string]*deviceLoginTransaction)}
}

func (s *deviceLoginStore) startJanitor() {
	s.janitorOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for now := range ticker.C {
				s.expire(now)
			}
		}()
	})
}

func (s *deviceLoginStore) reserve() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(time.Now())
	if len(s.transactions)+s.reservations >= maxPendingDeviceLogins {
		return false
	}
	s.reservations++
	return true
}

func (s *deviceLoginStore) releaseReservation() {
	s.mu.Lock()
	if s.reservations > 0 {
		s.reservations--
	}
	s.mu.Unlock()
}

func (s *deviceLoginStore) add(transaction *deviceLoginTransaction) {
	s.mu.Lock()
	if s.reservations > 0 {
		s.reservations--
	}
	s.transactions[transaction.proof] = transaction
	s.mu.Unlock()
}

func (s *deviceLoginStore) cancel(proof string) {
	s.mu.Lock()
	transaction, ok := s.transactions[proof]
	if ok {
		delete(s.transactions, proof)
	}
	s.mu.Unlock()
	if ok {
		transaction.cancel()
	}
}

func (s *deviceLoginStore) status(proof string) deviceLoginStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(time.Now())
	transaction, ok := s.transactions[proof]
	if !ok {
		return deviceLoginExpired
	}
	return transaction.status
}

func (s *deviceLoginStore) complete(proof string, token *oauth2.Token, err error) {
	s.mu.Lock()
	transaction, ok := s.transactions[proof]
	if !ok {
		s.mu.Unlock()
		return
	}
	if err == nil {
		transaction.status = deviceLoginApproved
		transaction.token = token
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || time.Now().After(transaction.expiresAt) {
		transaction.status = deviceLoginExpired
	} else {
		transaction.status = deviceLoginFailed
	}
	s.mu.Unlock()
}

func (s *deviceLoginStore) takeApproved(proof string) (*oauth2.Token, deviceLoginStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	transaction, ok := s.transactions[proof]
	if !ok {
		return nil, deviceLoginExpired
	}
	if time.Now().After(transaction.expiresAt) {
		delete(s.transactions, proof)
		transaction.cancel()
		return nil, deviceLoginExpired
	}
	if transaction.status != deviceLoginApproved {
		return nil, transaction.status
	}
	delete(s.transactions, proof)
	return transaction.token, deviceLoginApproved
}

func (s *deviceLoginStore) expire(now time.Time) {
	s.mu.Lock()
	s.expireLocked(now)
	s.mu.Unlock()
}

func (s *deviceLoginStore) expireLocked(now time.Time) {
	for proof, transaction := range s.transactions {
		if now.After(transaction.expiresAt) {
			delete(s.transactions, proof)
			transaction.cancel()
		}
	}
}

func init() {
	pendingDeviceLogins.startJanitor()
}

// IsOIDCDeviceLoginEnabled reports whether the opt-in device flow is available.
// The public config endpoint intentionally exposes only this boolean.
func IsOIDCDeviceLoginEnabled() bool {
	return oidcEnabled && database.IsMultiUserMode() && config.GetBool("OIDC_DEVICE_LOGIN_ENABLED", false) && oauth2Config != nil && oauth2Config.Endpoint.DeviceAuthURL != ""
}

func OIDCDeviceStartHandler(c *gin.Context) {
	if !IsOIDCDeviceLoginEnabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "OIDC device login is not enabled"})
		return
	}
	if !requireSameOrigin(c) {
		return
	}

	if oldProof, err := c.Cookie(deviceLoginProofCookie); err == nil {
		pendingDeviceLogins.cancel(oldProof)
	}
	if !pendingDeviceLogins.reserve() {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many pending device logins"})
		return
	}

	deviceAuth, err := oauth2Config.DeviceAuth(context.Background(), deviceLoginOAuthOptions()...)
	if err != nil {
		pendingDeviceLogins.releaseReservation()
		c.JSON(http.StatusBadGateway, gin.H{"error": "Device login provider unavailable"})
		return
	}
	if deviceAuth.VerificationURIComplete == "" {
		pendingDeviceLogins.releaseReservation()
		c.JSON(http.StatusBadGateway, gin.H{"error": "Device login provider did not return a verification link"})
		return
	}

	proof, err := newDeviceLoginProof()
	if err != nil {
		pendingDeviceLogins.releaseReservation()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start device login"})
		return
	}
	expiresAt := time.Now().Add(deviceLoginTTL)
	if !deviceAuth.Expiry.IsZero() && deviceAuth.Expiry.Before(expiresAt) {
		expiresAt = deviceAuth.Expiry
	}
	if !expiresAt.After(time.Now()) {
		pendingDeviceLogins.releaseReservation()
		c.JSON(http.StatusBadGateway, gin.H{"error": "Device login provider returned an expired request"})
		return
	}
	deviceAuth.Expiry = expiresAt
	ctx, cancel := context.WithDeadline(context.Background(), expiresAt)
	transaction := &deviceLoginTransaction{
		proof:     proof,
		device:    *deviceAuth,
		expiresAt: expiresAt,
		cancel:    cancel,
		status:    deviceLoginPending,
	}
	pendingDeviceLogins.add(transaction)
	go pollDeviceLogin(ctx, transaction)

	setDeviceLoginProofCookie(c, proof, expiresAt)
	c.JSON(http.StatusOK, gin.H{
		"status":                    deviceLoginPending,
		"verification_uri_complete": deviceAuth.VerificationURIComplete,
		"verification_uri":          deviceAuth.VerificationURI,
		"user_code":                 deviceAuth.UserCode,
	})
}

func OIDCDeviceStatusHandler(c *gin.Context) {
	if !IsOIDCDeviceLoginEnabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "OIDC device login is not enabled"})
		return
	}
	proof, err := c.Cookie(deviceLoginProofCookie)
	if err != nil || proof == "" {
		c.JSON(http.StatusNotFound, gin.H{"status": deviceLoginExpired})
		return
	}
	status := pendingDeviceLogins.status(proof)
	if status == deviceLoginExpired || status == deviceLoginFailed {
		clearDeviceLoginProofCookie(c)
	}
	c.JSON(http.StatusOK, gin.H{"status": status})
}

func OIDCDeviceFinishHandler(c *gin.Context) {
	if !IsOIDCDeviceLoginEnabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "OIDC device login is not enabled"})
		return
	}
	if !requireSameOrigin(c) {
		return
	}
	proof, err := c.Cookie(deviceLoginProofCookie)
	if err != nil || proof == "" {
		c.JSON(http.StatusNotFound, gin.H{"status": deviceLoginExpired})
		return
	}
	token, status := pendingDeviceLogins.takeApproved(proof)
	if status == deviceLoginPending {
		c.JSON(http.StatusAccepted, gin.H{"status": status})
		return
	}
	if status != deviceLoginApproved || token == nil {
		clearDeviceLoginProofCookie(c)
		c.JSON(http.StatusGone, gin.H{"status": status})
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		clearDeviceLoginProofCookie(c)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Device login provider returned no ID token"})
		return
	}
	identity, err := verifyOIDCIdentity(context.Background(), rawIDToken, "")
	if err != nil {
		clearDeviceLoginProofCookie(c)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Failed to verify device login"})
		return
	}
	if !database.IsMultiUserMode() {
		clearDeviceLoginProofCookie(c)
		c.JSON(http.StatusNotImplemented, gin.H{"error": "OIDC authentication requires multi-user mode"})
		return
	}
	user, err := authenticateOIDCMultiUser(identity.Username, identity.Email, identity.Name, identity.Subject, identity.Groups)
	if err != nil {
		clearDeviceLoginProofCookie(c)
		if err.Error() == "account disabled" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "backend.auth.account_disabled"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	if err := issueOIDCSession(c, user, rawIDToken); err != nil {
		clearDeviceLoginProofCookie(c)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create session"})
		return
	}
	clearDeviceLoginProofCookie(c)
	c.JSON(http.StatusOK, gin.H{"authenticated": true})
}

func OIDCDeviceCancelHandler(c *gin.Context) {
	if !IsOIDCDeviceLoginEnabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "OIDC device login is not enabled"})
		return
	}
	if !requireSameOrigin(c) {
		return
	}
	if proof, err := c.Cookie(deviceLoginProofCookie); err == nil && proof != "" {
		pendingDeviceLogins.cancel(proof)
	}
	clearDeviceLoginProofCookie(c)
	c.JSON(http.StatusOK, gin.H{"status": deviceLoginExpired})
}

func pollDeviceLogin(ctx context.Context, transaction *deviceLoginTransaction) {
	token, err := oauth2Config.DeviceAccessToken(ctx, &transaction.device, deviceLoginOAuthOptions()...)
	pendingDeviceLogins.complete(transaction.proof, token, err)
}

func deviceLoginOAuthOptions() []oauth2.AuthCodeOption {
	if oauth2Config == nil || oauth2Config.ClientSecret == "" {
		return nil
	}
	return []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("client_secret", oauth2Config.ClientSecret)}
}

func newDeviceLoginProof() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func setDeviceLoginProofCookie(c *gin.Context, proof string, expiresAt time.Time) {
	secure := !allowInsecure()
	c.SetSameSite(http.SameSiteStrictMode)
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	c.SetCookie(deviceLoginProofCookie, proof, maxAge, "/api/auth/oidc/device", "", secure, true)
}

func clearDeviceLoginProofCookie(c *gin.Context) {
	secure := !allowInsecure()
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(deviceLoginProofCookie, "", -1, "/api/auth/oidc/device", "", secure, true)
}

func requireSameOrigin(c *gin.Context) bool {
	origin := strings.TrimSpace(c.GetHeader("Origin"))
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host != c.Request.Host {
		c.JSON(http.StatusForbidden, gin.H{"error": "Origin check failed"})
		return false
	}
	return true
}
