package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	jwtIssuer   = "aevor"
	jwtAudience = "aevor-api"
	defaultTTL  = 7 * 24 * time.Hour
)

var (
	ErrInvalidToken     = errors.New("invalid token")
	ErrInvalidJWTSecret = errors.New("jwt signing secret must be at least 32 bytes")
)

type JWTManager struct {
	secret   []byte
	issuer   string
	audience string
	now      func() time.Time
}

type JWTManagerOption func(*JWTManager)

func WithClock(now func() time.Time) JWTManagerOption {
	return func(m *JWTManager) {
		if now != nil {
			m.now = now
		}
	}
}

func NewJWTManager(secret []byte, opts ...JWTManagerOption) *JWTManager {
	m := &JWTManager{
		secret:   secret,
		issuer:   jwtIssuer,
		audience: jwtAudience,
		now:      time.Now,
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

func (m *JWTManager) Issue(userID uuid.UUID, ttl time.Duration) (string, error) {
	if len(m.secret) < 32 {
		return "", ErrInvalidJWTSecret
	}

	now := m.now()

	claims := jwt.RegisteredClaims{
		Issuer:    m.issuer,
		Audience:  jwt.ClaimStrings{m.audience},
		Subject:   userID.String(),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	return token.SignedString(m.secret)
}

func (m *JWTManager) Verify(tokenString string) (uuid.UUID, error) {
	claims := &jwt.RegisteredClaims{}

	_, err := jwt.ParseWithClaims(
		tokenString,
		claims,
		func(token *jwt.Token) (any, error) {
			return m.secret, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(m.issuer),
		jwt.WithAudience(m.audience),
		jwt.WithExpirationRequired(),
	)

	if err != nil {
		return uuid.Nil, ErrInvalidToken
	}

	if claims.Subject == "" {
		return uuid.Nil, ErrInvalidToken
	}

	userID, err := uuid.Parse(claims.Subject)

	if err != nil {
		return uuid.Nil, ErrInvalidToken
	}

	return userID, nil
}
