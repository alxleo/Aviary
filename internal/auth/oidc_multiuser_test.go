package auth

import (
	"fmt"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/rmitchellscott/aviary/internal/database"
	"gorm.io/gorm"
)

func oidcMultiUserTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:oidc-multiuser-test-%s?mode=memory&cache=shared", uuid.NewString())), &gorm.Config{})
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

func TestAuthenticateOIDCNoEmailUsesDistinctSubjectEmails(t *testing.T) {
	t.Setenv("OIDC_AUTO_CREATE_USERS", "true")
	t.Setenv("OIDC_AUTO_LINK_USERS", "false")
	t.Setenv("OIDC_ADMIN_GROUP", "oidc-admin")
	oidcMultiUserTestDB(t)

	first, err := authenticateOIDCMultiUser("alex-one", "", "", "subject-one", nil)
	if err != nil {
		t.Fatalf("first authentication: %v", err)
	}
	second, err := authenticateOIDCMultiUser("alex-two", "", "", "subject-two", nil)
	if err != nil {
		t.Fatalf("second authentication: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("different subjects with no email claim resolved to the same user")
	}
	if first.Email == "" || second.Email == "" || first.Email == second.Email {
		t.Fatalf("synthetic emails = %q and %q, want distinct non-empty values", first.Email, second.Email)
	}
	if !strings.HasSuffix(first.Email, "@oidc.invalid") || !strings.HasSuffix(second.Email, "@oidc.invalid") {
		t.Fatalf("synthetic emails = %q and %q, want @oidc.invalid values", first.Email, second.Email)
	}
	if expected, _ := syntheticOIDCEmail("subject-one"); first.Email != expected {
		t.Fatalf("first synthetic email = %q, want stable value %q", first.Email, expected)
	}
}

func TestAuthenticateOIDCUnboundUsernameCollisionCannotClaimWhenAutoLinkDisabled(t *testing.T) {
	t.Setenv("OIDC_AUTO_CREATE_USERS", "true")
	t.Setenv("OIDC_AUTO_LINK_USERS", "false")
	t.Setenv("OIDC_ADMIN_GROUP", "oidc-admin")
	db := oidcMultiUserTestDB(t)
	local := database.User{
		ID:       uuid.New(),
		Username: "alex",
		Email:    "alex@example.test",
		Password: "local-password",
		IsActive: true,
	}
	if err := db.Create(&local).Error; err != nil {
		t.Fatalf("create local user: %v", err)
	}

	if _, err := authenticateOIDCMultiUser("alex", "oidc@example.test", "", "new-subject", nil); err == nil {
		t.Fatal("authentication succeeded by claiming an unbound username while auto-linking is disabled")
	}

	var stored database.User
	if err := db.First(&stored, "id = ?", local.ID).Error; err != nil {
		t.Fatalf("reload local user: %v", err)
	}
	if stored.OidcSubject != nil && *stored.OidcSubject != "" {
		t.Fatalf("local user was linked to OIDC subject %q", *stored.OidcSubject)
	}
}

func TestAuthenticateOIDCExistingSubjectWorksWhenAutoLinkDisabled(t *testing.T) {
	t.Setenv("OIDC_AUTO_CREATE_USERS", "false")
	t.Setenv("OIDC_AUTO_LINK_USERS", "false")
	t.Setenv("OIDC_ADMIN_GROUP", "oidc-admin")
	db := oidcMultiUserTestDB(t)
	subject := "alex-subject"
	linked := database.User{
		ID:          uuid.New(),
		Username:    "alex",
		Email:       "alex@example.test",
		Password:    "local-password",
		IsActive:    true,
		OidcSubject: &subject,
	}
	if err := db.Create(&linked).Error; err != nil {
		t.Fatalf("create linked user: %v", err)
	}

	user, err := authenticateOIDCMultiUser("alex-renamed", "alex-renamed@example.test", "", subject, nil)
	if err != nil {
		t.Fatalf("authenticate existing subject: %v", err)
	}
	if user.ID != linked.ID {
		t.Fatalf("resolved user ID = %s, want %s", user.ID, linked.ID)
	}
}

func TestAuthenticateOIDCAutoLinkDefaultsToEnabled(t *testing.T) {
	t.Setenv("OIDC_AUTO_CREATE_USERS", "false")
	t.Setenv("OIDC_AUTO_LINK_USERS", "")
	t.Setenv("OIDC_ADMIN_GROUP", "oidc-admin")
	db := oidcMultiUserTestDB(t)
	local := database.User{
		ID:       uuid.New(),
		Username: "legacy-alex",
		Email:    "legacy@example.test",
		Password: "local-password",
		IsActive: true,
	}
	if err := db.Create(&local).Error; err != nil {
		t.Fatalf("create local user: %v", err)
	}

	user, err := authenticateOIDCMultiUser("legacy-alex", "new@example.test", "", "legacy-subject", nil)
	if err != nil {
		t.Fatalf("legacy auto-link authentication: %v", err)
	}
	if user.ID != local.ID {
		t.Fatalf("resolved user ID = %s, want legacy user %s", user.ID, local.ID)
	}
	if user.OidcSubject == nil || *user.OidcSubject != "legacy-subject" {
		t.Fatalf("OIDC subject = %v, want legacy-subject", user.OidcSubject)
	}
}
