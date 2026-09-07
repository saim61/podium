package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/auth"
	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/games"
	"github.com/saim61/podium/internal/httpapi"
	"github.com/saim61/podium/internal/ratelimit"
	"github.com/saim61/podium/internal/session"
	"github.com/saim61/podium/internal/testsupport"
)

type authHarness struct {
	router   http.Handler
	service  *auth.Service
	sessions *session.Service
	pool     *pgxpool.Pool
	cfg      config.Config
	holds    *holdRecorder
}

// holdRecorder stands in for the delay reaction time needs. It records what the engine asked
// for and returns immediately, so the suite does not spend three seconds per round. One test
// uses the real timer to prove the wait genuinely happens.
type holdRecorder struct {
	mu        sync.Mutex
	requested []time.Time
	honour    bool
}

func (h *holdRecorder) sleep(ctx context.Context, until time.Time) error {
	h.mu.Lock()
	h.requested = append(h.requested, until)
	honour := h.honour
	h.mu.Unlock()

	if !honour {
		return nil
	}

	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *holdRecorder) all() []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]time.Time(nil), h.requested...)
}

type harnessOption func(*holdRecorder)

// honourHolds makes the harness actually wait for a held response.
func honourHolds(h *holdRecorder) { h.honour = true }

func passwordHash(t *testing.T, pool *pgxpool.Pool, username string) string {
	t.Helper()

	var hash string
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT password_hash FROM users WHERE username = $1", username).Scan(&hash))
	return hash
}

func newAuthHarness(t *testing.T, opts ...harnessOption) *authHarness {
	t.Helper()

	// Production Argon2 cost is 64 MiB per hash; at that setting this file would take minutes.
	t.Setenv("PODIUM_ARGON2_MEMORY_KIB", "8192")
	t.Setenv("PODIUM_ARGON2_ITERATIONS", "1")
	t.Setenv("PODIUM_ARGON2_PARALLELISM", "1")

	pool := testsupport.Postgres(t)
	rdb := testsupport.Redis(t)

	cfg, err := config.Load()
	require.NoError(t, err)

	service, err := auth.NewService(pool, cfg.Auth)
	require.NoError(t, err)

	limiter, err := ratelimit.New(rdb)
	require.NoError(t, err)

	holds := &holdRecorder{}
	for _, opt := range opts {
		opt(holds)
	}

	sessions := session.NewService(pool, games.NewRegistry(), session.WithSleeper(holds.sleep))

	return &authHarness{
		router: httpapi.NewRouter(httpapi.Deps{
			Config:   cfg,
			Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
			Auth:     service,
			Sessions: sessions,
			Limiter:  limiter,
		}),
		service:  service,
		sessions: sessions,
		pool:     pool,
		cfg:      cfg,
		holds:    holds,
	}
}

func (h *authHarness) do(t *testing.T, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	}

	r := httptest.NewRequest(method, path, reader)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, r)
	return rec
}

type tokensBody struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
}

type authBody struct {
	User struct {
		Username  string    `json:"username"`
		Email     string    `json:"email"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"user"`
	Tokens tokensBody `json:"tokens"`
}

type errBody struct {
	Error struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Fields  map[string]string `json:"fields"`
	} `json:"error"`
}

func decodeInto[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var out T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return out
}

func registration(username string) map[string]string {
	return map[string]string{
		"username": username,
		"email":    username + "@example.com",
		"password": "correct horse battery",
	}
}

func (h *authHarness) register(t *testing.T, username string) authBody {
	t.Helper()

	rec := h.do(t, http.MethodPost, "/v1/auth/register", registration(username), "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return decodeInto[authBody](t, rec)
}

func TestRegisterCreatesAccountAndSignsIn(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodPost, "/v1/auth/register", registration("saeem"), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	body := decodeInto[authBody](t, rec)
	require.Equal(t, "saeem", body.User.Username)
	require.Equal(t, "saeem@example.com", body.User.Email)
	require.False(t, body.User.CreatedAt.IsZero())
	require.NotEmpty(t, body.Tokens.AccessToken)
	require.NotEmpty(t, body.Tokens.RefreshToken)
	require.Equal(t, "Bearer", body.Tokens.TokenType)
	require.Positive(t, body.Tokens.ExpiresIn)
}

func TestRegisterNeverLeaksPasswordOrInternalID(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodPost, "/v1/auth/register", registration("saeem"), "")

	raw := rec.Body.String()
	require.NotContains(t, raw, "correct horse battery")
	require.NotContains(t, raw, "argon2")
	require.NotContains(t, raw, "password_hash")
	require.NotContains(t, raw, `"id"`, "the internal numeric id must not be exposed")
}

func TestRegisterRejectsDuplicateUsernameRegardlessOfCase(t *testing.T) {
	h := newAuthHarness(t)
	h.register(t, "saeem")

	duplicate := registration("SAEEM")
	duplicate["email"] = "different@example.com"

	rec := h.do(t, http.MethodPost, "/v1/auth/register", duplicate, "")

	require.Equal(t, http.StatusConflict, rec.Code)
	body := decodeInto[errBody](t, rec)
	require.Equal(t, "conflict", body.Error.Code)
	require.Contains(t, body.Error.Fields, "username")
}

func TestRegisterRejectsDuplicateEmailRegardlessOfCase(t *testing.T) {
	h := newAuthHarness(t)
	h.register(t, "saeem")

	duplicate := registration("someone")
	duplicate["email"] = "SAEEM@EXAMPLE.COM"

	rec := h.do(t, http.MethodPost, "/v1/auth/register", duplicate, "")

	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, decodeInto[errBody](t, rec).Error.Fields, "email")
}

func TestRegisterReportsEveryInvalidFieldAtOnce(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodPost, "/v1/auth/register", map[string]string{
		"username": "x",
		"email":    "nope",
		"password": "short",
	}, "")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	body := decodeInto[errBody](t, rec)
	require.Len(t, body.Error.Fields, 3)
}

func TestLoginWithUsernameOrEmail(t *testing.T) {
	h := newAuthHarness(t)
	h.register(t, "saeem")

	for _, login := range []string{"saeem", "SAEEM", "saeem@example.com", "SAEEM@EXAMPLE.COM"} {
		rec := h.do(t, http.MethodPost, "/v1/auth/login", map[string]string{
			"login":    login,
			"password": "correct horse battery",
		}, "")

		require.Equal(t, http.StatusOK, rec.Code, login)
		require.NotEmpty(t, decodeInto[authBody](t, rec).Tokens.AccessToken)
	}
}

// The two failure modes must be indistinguishable, or the endpoint becomes a way to enumerate
// which accounts exist.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	h := newAuthHarness(t)
	h.register(t, "saeem")

	wrongPassword := h.do(t, http.MethodPost, "/v1/auth/login", map[string]string{
		"login":    "saeem",
		"password": "not the password",
	}, "")

	noSuchUser := h.do(t, http.MethodPost, "/v1/auth/login", map[string]string{
		"login":    "nobody",
		"password": "not the password",
	}, "")

	require.Equal(t, http.StatusUnauthorized, wrongPassword.Code)
	require.Equal(t, http.StatusUnauthorized, noSuchUser.Code)
	require.Equal(t,
		decodeInto[errBody](t, wrongPassword).Error.Message,
		decodeInto[errBody](t, noSuchUser).Error.Message)
}

func TestMeRequiresAuthentication(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodGet, "/v1/me", nil, "")

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, `Bearer realm="podium"`, rec.Header().Get("WWW-Authenticate"))
}

func TestMeReturnsTheAuthenticatedUser(t *testing.T) {
	h := newAuthHarness(t)
	registered := h.register(t, "saeem")

	rec := h.do(t, http.MethodGet, "/v1/me", nil, registered.Tokens.AccessToken)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "saeem")
}

func TestMeRejectsInvalidTokens(t *testing.T) {
	h := newAuthHarness(t)

	for name, token := range map[string]string{
		"garbage":       "not-a-token",
		"empty bearer":  " ",
		"random base64": "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.bm90YXNpZw",
	} {
		t.Run(name, func(t *testing.T) {
			rec := h.do(t, http.MethodGet, "/v1/me", nil, token)
			require.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

func TestMeRejectsExpiredAccessToken(t *testing.T) {
	h := newAuthHarness(t)

	// A service whose clock is two hours behind issues a token that is already expired by the
	// time the router, running on the real clock, sees it. It shares the harness pool: calling
	// testsupport.Postgres again would truncate the tables underneath this test.
	past, err := auth.NewService(h.pool, h.cfg.Auth,
		auth.WithClock(func() time.Time { return time.Now().Add(-2 * time.Hour) }))
	require.NoError(t, err)

	_, pair, err := past.Register(t.Context(), auth.Registration{
		Username: "stale", Email: "stale@example.com", Password: "correct horse battery",
	})
	require.NoError(t, err)

	rec := h.do(t, http.MethodGet, "/v1/me", nil, pair.AccessToken)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, decodeInto[errBody](t, rec).Error.Message, "expired")
}

func TestRefreshRotatesTheToken(t *testing.T) {
	h := newAuthHarness(t)
	registered := h.register(t, "saeem")

	rec := h.do(t, http.MethodPost, "/v1/auth/refresh", map[string]string{
		"refresh_token": registered.Tokens.RefreshToken,
	}, "")

	require.Equal(t, http.StatusOK, rec.Code)
	rotated := decodeInto[tokensBody](t, rec)
	require.NotEmpty(t, rotated.AccessToken)
	require.NotEqual(t, registered.Tokens.RefreshToken, rotated.RefreshToken,
		"the refresh token must be replaced, not reissued")

	// The new access token has to actually work.
	me := h.do(t, http.MethodGet, "/v1/me", nil, rotated.AccessToken)
	require.Equal(t, http.StatusOK, me.Code)
}

// The property this whole design exists for: a stolen refresh token cannot be used alongside the
// legitimate one without both being cut off.
func TestRefreshReuseRevokesTheWholeFamily(t *testing.T) {
	h := newAuthHarness(t)
	registered := h.register(t, "saeem")
	stolen := registered.Tokens.RefreshToken

	first := h.do(t, http.MethodPost, "/v1/auth/refresh",
		map[string]string{"refresh_token": stolen}, "")
	require.Equal(t, http.StatusOK, first.Code)
	rotated := decodeInto[tokensBody](t, first)

	replay := h.do(t, http.MethodPost, "/v1/auth/refresh",
		map[string]string{"refresh_token": stolen}, "")
	require.Equal(t, http.StatusUnauthorized, replay.Code)
	require.Contains(t, decodeInto[errBody](t, replay).Error.Message, "already been used")

	// The token issued by the legitimate rotation is now dead too. That is the point: Podium
	// cannot tell the thief from the victim, so it ends the session and forces a fresh login.
	afterRevocation := h.do(t, http.MethodPost, "/v1/auth/refresh",
		map[string]string{"refresh_token": rotated.RefreshToken}, "")
	require.Equal(t, http.StatusUnauthorized, afterRevocation.Code)
}

func TestRefreshRejectsUnknownToken(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodPost, "/v1/auth/refresh",
		map[string]string{"refresh_token": strings.Repeat("a", 43)}, "")

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, decodeInto[errBody](t, rec).Error.Message, "not valid")
}

func TestRefreshRequiresAToken(t *testing.T) {
	h := newAuthHarness(t)

	rec := h.do(t, http.MethodPost, "/v1/auth/refresh", map[string]string{}, "")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, decodeInto[errBody](t, rec).Error.Fields, "refresh_token")
}

func TestLogoutEndsTheSession(t *testing.T) {
	h := newAuthHarness(t)
	registered := h.register(t, "saeem")

	logout := h.do(t, http.MethodPost, "/v1/auth/logout",
		map[string]string{"refresh_token": registered.Tokens.RefreshToken}, "")
	require.Equal(t, http.StatusNoContent, logout.Code)

	after := h.do(t, http.MethodPost, "/v1/auth/refresh",
		map[string]string{"refresh_token": registered.Tokens.RefreshToken}, "")
	require.Equal(t, http.StatusUnauthorized, after.Code)
}

func TestLogoutIsIdempotentAndDoesNotRevealUnknownTokens(t *testing.T) {
	h := newAuthHarness(t)
	registered := h.register(t, "saeem")

	for range 2 {
		rec := h.do(t, http.MethodPost, "/v1/auth/logout",
			map[string]string{"refresh_token": registered.Tokens.RefreshToken}, "")
		require.Equal(t, http.StatusNoContent, rec.Code)
	}

	unknown := h.do(t, http.MethodPost, "/v1/auth/logout",
		map[string]string{"refresh_token": strings.Repeat("z", 43)}, "")
	require.Equal(t, http.StatusNoContent, unknown.Code)
}

// Documented tradeoff, asserted so it cannot change by accident: logout revokes the refresh
// family but cannot un-issue a signed JWT. The access token stays usable until it expires.
func TestAccessTokenSurvivesLogoutUntilExpiry(t *testing.T) {
	h := newAuthHarness(t)
	registered := h.register(t, "saeem")

	logout := h.do(t, http.MethodPost, "/v1/auth/logout",
		map[string]string{"refresh_token": registered.Tokens.RefreshToken}, "")
	require.Equal(t, http.StatusNoContent, logout.Code)

	me := h.do(t, http.MethodGet, "/v1/me", nil, registered.Tokens.AccessToken)
	require.Equal(t, http.StatusOK, me.Code,
		"a stateless access token cannot be revoked; the 15 minute TTL is the bound")
}

func TestLoginThrottlesRepeatedFailuresPerAccount(t *testing.T) {
	h := newAuthHarness(t)
	h.register(t, "saeem")

	limit := h.cfg.Auth.LoginMaxAccount
	wrong := map[string]string{"login": "saeem", "password": "wrong password here"}

	for attempt := 1; attempt <= limit; attempt++ {
		rec := h.do(t, http.MethodPost, "/v1/auth/login", wrong, "")
		require.Equal(t, http.StatusUnauthorized, rec.Code, "attempt %d", attempt)
	}

	blocked := h.do(t, http.MethodPost, "/v1/auth/login", wrong, "")
	require.Equal(t, http.StatusTooManyRequests, blocked.Code)
	require.NotEmpty(t, blocked.Header().Get("Retry-After"))
	require.Equal(t, "rate_limited", decodeInto[errBody](t, blocked).Error.Code)

	// Even the correct password is refused while the window is open.
	correct := h.do(t, http.MethodPost, "/v1/auth/login", map[string]string{
		"login": "saeem", "password": "correct horse battery",
	}, "")
	require.Equal(t, http.StatusTooManyRequests, correct.Code)
}

func TestSuccessfulLoginClearsFailureCounter(t *testing.T) {
	h := newAuthHarness(t)
	h.register(t, "saeem")

	for range h.cfg.Auth.LoginMaxAccount - 1 {
		rec := h.do(t, http.MethodPost, "/v1/auth/login", map[string]string{
			"login": "saeem", "password": "wrong password here",
		}, "")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}

	ok := h.do(t, http.MethodPost, "/v1/auth/login", map[string]string{
		"login": "saeem", "password": "correct horse battery",
	}, "")
	require.Equal(t, http.StatusOK, ok.Code)

	// The counter was reset, so a fresh run of failures is available rather than one attempt.
	for attempt := range h.cfg.Auth.LoginMaxAccount {
		rec := h.do(t, http.MethodPost, "/v1/auth/login", map[string]string{
			"login": "saeem", "password": "wrong password here",
		}, "")
		require.Equal(t, http.StatusUnauthorized, rec.Code, "attempt %d after reset", attempt)
	}
}

func TestLoginReportsRateLimitHeaders(t *testing.T) {
	h := newAuthHarness(t)
	h.register(t, "saeem")

	rec := h.do(t, http.MethodPost, "/v1/auth/login", map[string]string{
		"login": "saeem", "password": "correct horse battery",
	}, "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEmpty(t, rec.Header().Get("RateLimit-Limit"))
	require.NotEmpty(t, rec.Header().Get("RateLimit-Remaining"))
}

func TestPasswordIsRehashedWhenCostIncreases(t *testing.T) {
	pool := testsupport.Postgres(t)

	weak := config.Argon2{MemoryKiB: 8192, Iterations: 1, Parallelism: 1}
	strong := config.Argon2{MemoryKiB: 16384, Iterations: 2, Parallelism: 2}

	cfg, err := config.Load()
	require.NoError(t, err)

	cfg.Auth.Argon2 = weak
	weakService, err := auth.NewService(pool, cfg.Auth)
	require.NoError(t, err)

	_, _, err = weakService.Register(t.Context(), auth.Registration{
		Username: "saeem", Email: "saeem@example.com", Password: "correct horse battery",
	})
	require.NoError(t, err)

	storedBefore := passwordHash(t, pool, "saeem")
	require.Contains(t, storedBefore, "m=8192,t=1,p=1")

	cfg.Auth.Argon2 = strong
	strongService, err := auth.NewService(pool, cfg.Auth)
	require.NoError(t, err)

	_, _, err = strongService.Login(t.Context(), "saeem", "correct horse battery")
	require.NoError(t, err)

	require.Contains(t, passwordHash(t, pool, "saeem"), "m=16384,t=2,p=2",
		"a successful login should upgrade a hash stored at an older cost")
}
