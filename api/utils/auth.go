package utils

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ErrAuthNotConfigured is returned when session validation is requested
// but Postgres isn't initialized (no DATABASE_URL). Middlewares map this
// to 503 — server-side misconfiguration, not a client error.
var ErrAuthNotConfigured = errors.New("database not configured")

// ErrInvalidSession means the token is missing/unknown/expired.
var ErrInvalidSession = errors.New("invalid or expired session")

// AuthUser is the project-friendly view of a Better Auth user. The
// middleware stashes this in the gin context once per request so
// services can read it without re-querying.
type AuthUser struct {
	ID            string
	Email         string
	Name          string
	EmailVerified bool
}

// GetUserFromSession validates a Better Auth session token by looking it
// up in the shared Postgres `session` table and joining the `user` row.
// Better Auth is the source of truth — it created the session on login;
// the API never signs or verifies a JWT itself.
//
// Better Auth signs the session cookie as `<token>.<hmac>`; we only need
// the high-entropy token portion for the DB lookup (the row's existence
// + expiry is the authority), so we split on the first ".".
//
// Returns ErrAuthNotConfigured when Postgres isn't wired up; any other
// non-nil error means the caller should respond 401.
func GetUserFromSession(rawToken string) (*AuthUser, error) {
	if DB == nil {
		return nil, ErrAuthNotConfigured
	}
	token := rawToken
	if i := strings.IndexByte(token, '.'); i >= 0 {
		token = token[:i]
	}
	if token == "" {
		return nil, ErrInvalidSession
	}

	var sess BetterAuthSession
	err := DB.Where("token = ?", token).First(&sess).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInvalidSession
	}
	if err != nil {
		return nil, err
	}
	if !sess.ExpiresAt.IsZero() && time.Now().After(sess.ExpiresAt) {
		return nil, ErrInvalidSession
	}

	var usr BetterAuthUser
	if err := DB.Where("id = ?", sess.UserID).First(&usr).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidSession
		}
		return nil, err
	}
	return &AuthUser{
		ID:            usr.ID,
		Email:         usr.Email,
		Name:          usr.Name,
		EmailVerified: usr.EmailVerified,
	}, nil
}
