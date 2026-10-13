// Package token issues and verifies short-lived access JWTs and opaque refresh tokens.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type Claims struct {
	CompanyID uuid.UUID `json:"cid"`
	jwt.RegisteredClaims
}

type Manager struct {
	secret []byte
	ttl    time.Duration
}

func NewManager(secret string, accessTTL time.Duration) *Manager {
	return &Manager{secret: []byte(secret), ttl: accessTTL}
}

func (m *Manager) TTL() time.Duration { return m.ttl }

func (m *Manager) IssueAccess(userID, companyID uuid.UUID, now time.Time) (string, time.Time, error) {
	exp := now.Add(m.ttl)
	c := Claims{
		CompanyID: companyID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: userID.String(), ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(m.secret)
	return s, exp, err
}

func (m *Manager) ParseAccess(s string) (userID, companyID uuid.UUID, err error) {
	var c Claims
	t, err := jwt.ParseWithClaims(s, &c, func(*jwt.Token) (any, error) { return m.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !t.Valid {
		return uuid.Nil, uuid.Nil, errors.New("invalid access token")
	}
	userID, err = uuid.Parse(c.Subject)
	if err != nil {
		return uuid.Nil, uuid.Nil, errors.New("invalid subject")
	}
	return userID, c.CompanyID, nil
}

// NewRefresh returns an opaque token and its SHA-256 hash (only the hash is stored).
func NewRefresh() (token string, hash []byte) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashRefresh(token)
}

func HashRefresh(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}
