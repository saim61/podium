package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/saim61/podium/internal/config"
)

const refreshTokenBytes = 32

// TokenIssuer signs and verifies short-lived access tokens.
type TokenIssuer struct {
	secret []byte
	issuer string
	ttl    time.Duration
	now    func() time.Time
}

// NewTokenIssuer builds an issuer from configuration.
func NewTokenIssuer(cfg config.Auth) *TokenIssuer {
	return &TokenIssuer{
		secret: []byte(cfg.JWTSecret),
		issuer: cfg.Issuer,
		ttl:    cfg.AccessTokenTTL,
		now:    time.Now,
	}
}

// AccessClaims is the verified content of an access token.
type AccessClaims struct {
	UserID    int64
	TokenID   string
	ExpiresAt time.Time
}

// Issue signs an access token for a user and reports when it expires.
func (i *TokenIssuer) Issue(userID int64) (string, time.Time, error) {
	now := i.now()
	expiresAt := now.Add(i.ttl)

	claims := jwt.RegisteredClaims{
		Subject:   strconv.FormatInt(userID, 10),
		Issuer:    i.issuer,
		ID:        uuid.NewString(),
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// Verify checks an access token's signature, issuer and expiry.
//
// WithValidMethods pins the algorithm to HS256. Without it a token carrying "alg": "none", or an
// RS256 token verified against this HMAC secret as if it were a public key, would be accepted -
// the classic JWT algorithm confusion attack.
func (i *TokenIssuer) Verify(raw string) (AccessClaims, error) {
	var claims jwt.RegisteredClaims

	_, err := jwt.ParseWithClaims(raw, &claims,
		func(*jwt.Token) (any, error) { return i.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(i.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(i.now),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return AccessClaims{}, ErrTokenExpired
		}
		return AccessClaims{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || userID <= 0 {
		return AccessClaims{}, fmt.Errorf("%w: subject is not a user id", ErrInvalidToken)
	}

	return AccessClaims{
		UserID:    userID,
		TokenID:   claims.ID,
		ExpiresAt: claims.ExpiresAt.Time,
	}, nil
}

// newRefreshToken returns an opaque token and the hash to store for it.
func newRefreshToken() (string, []byte, error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate refresh token: %w", err)
	}

	raw := base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashRefreshToken(raw), nil
}

// hashRefreshToken hashes a refresh token for storage.
//
// SHA-256 rather than Argon2: a refresh token is 256 bits of output from a CSPRNG, so there is no
// low-entropy guess space to slow an attacker down in. Argon2 here would add its full cost to
// every refresh and buy nothing. Hashing at all is what matters - it means a leaked database
// cannot be replayed as a set of live sessions.
func hashRefreshToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
