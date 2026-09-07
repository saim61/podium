package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/config"
)

func testAuthConfig() config.Auth {
	return config.Auth{
		JWTSecret:      strings.Repeat("k", 32),
		Issuer:         "podium",
		AccessTokenTTL: 15 * time.Minute,
	}
}

func TestIssueAndVerify(t *testing.T) {
	issuer := NewTokenIssuer(testAuthConfig())

	token, expiresAt, err := issuer.Issue(42)
	require.NoError(t, err)
	require.NotEmpty(t, token)
	require.WithinDuration(t, time.Now().Add(15*time.Minute), expiresAt, time.Minute)

	claims, err := issuer.Verify(token)
	require.NoError(t, err)
	require.Equal(t, int64(42), claims.UserID)
	require.NotEmpty(t, claims.TokenID)
}

func TestVerifyRejectsAnotherSecret(t *testing.T) {
	issuer := NewTokenIssuer(testAuthConfig())

	token, _, err := issuer.Issue(1)
	require.NoError(t, err)

	other := testAuthConfig()
	other.JWTSecret = strings.Repeat("x", 32)

	_, err = NewTokenIssuer(other).Verify(token)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyRejectsAnotherIssuer(t *testing.T) {
	issuer := NewTokenIssuer(testAuthConfig())

	token, _, err := issuer.Issue(1)
	require.NoError(t, err)

	other := testAuthConfig()
	other.Issuer = "somebody-else"

	_, err = NewTokenIssuer(other).Verify(token)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyReportsExpiry(t *testing.T) {
	issuer := NewTokenIssuer(testAuthConfig())

	past := time.Now().Add(-time.Hour)
	issuer.now = func() time.Time { return past }

	token, _, err := issuer.Issue(1)
	require.NoError(t, err)

	issuer.now = time.Now
	_, err = issuer.Verify(token)

	require.ErrorIs(t, err, ErrTokenExpired)
}

// The algorithm confusion attack: a token claiming "alg": "none" carries no signature at all. A
// verifier that reads the algorithm from the token rather than pinning it accepts anything.
func TestVerifyRejectsUnsignedToken(t *testing.T) {
	issuer := NewTokenIssuer(testAuthConfig())

	b64 := base64.RawURLEncoding
	header := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))

	claims, err := json.Marshal(jwt.RegisteredClaims{
		Subject:   "1",
		Issuer:    "podium",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	require.NoError(t, err)

	forged := header + "." + b64.EncodeToString(claims) + "."

	_, err = issuer.Verify(forged)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyRejectsDifferentSigningAlgorithm(t *testing.T) {
	cfg := testAuthConfig()
	issuer := NewTokenIssuer(cfg)

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.RegisteredClaims{
		Subject:   "1",
		Issuer:    cfg.Issuer,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString([]byte(cfg.JWTSecret))
	require.NoError(t, err)

	_, err = issuer.Verify(signed)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyRejectsNonNumericSubject(t *testing.T) {
	cfg := testAuthConfig()
	issuer := NewTokenIssuer(cfg)

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "administrator",
		Issuer:    cfg.Issuer,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString([]byte(cfg.JWTSecret))
	require.NoError(t, err)

	_, err = issuer.Verify(signed)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyRejectsTokenWithoutExpiry(t *testing.T) {
	cfg := testAuthConfig()
	issuer := NewTokenIssuer(cfg)

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject: "1",
		Issuer:  cfg.Issuer,
	}).SignedString([]byte(cfg.JWTSecret))
	require.NoError(t, err)

	_, err = issuer.Verify(signed)
	require.ErrorIs(t, err, ErrInvalidToken)
}

func TestVerifyRejectsGarbage(t *testing.T) {
	issuer := NewTokenIssuer(testAuthConfig())

	for _, raw := range []string{"", "not.a.token", "a.b", strings.Repeat("x", 64)} {
		_, err := issuer.Verify(raw)
		require.ErrorIs(t, err, ErrInvalidToken, raw)
	}
}

func TestRefreshTokensAreUniqueAndHashed(t *testing.T) {
	first, firstHash, err := newRefreshToken()
	require.NoError(t, err)
	second, secondHash, err := newRefreshToken()
	require.NoError(t, err)

	require.NotEqual(t, first, second)
	require.NotEqual(t, firstHash, secondHash)
	require.Len(t, firstHash, 32)

	require.Equal(t, firstHash, hashRefreshToken(first), "hashing must be deterministic")
	require.NotContains(t, string(firstHash), first, "the stored hash must not contain the token")
}
