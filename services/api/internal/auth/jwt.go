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

var ErrInvalidToken = errors.New("invalid token")

type JWTManager struct {
	secret   []byte
	issuer   string
	audience string
	now      func() time.Time
}

func NewJWTManager(secret []byte) *JWTManager {
	return &JWTManager{
		secret:   secret,
		issuer:   jwtIssuer,
		audience: jwtAudience,
		now:      time.Now,
	}
}

func (m *JWTManager) Issue(userID uuid.UUID, ttl time.Duration) (string, error) {
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
