package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/saim61/podium/internal/auth"
	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/platform/observability"
	"github.com/saim61/podium/internal/ratelimit"
	"github.com/saim61/podium/internal/store/db"
)

type registerRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (r registerRequest) validate() error {
	f := fields{}
	validateUsername(f, r.Username)
	validateEmail(f, r.Email)
	validatePassword(f, r.Password, r.Username)
	return f.err("the registration details are invalid")
}

type loginRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

func (r loginRequest) validate() error {
	f := fields{}
	if r.Login == "" {
		f.add("login", "is required")
	}
	if r.Password == "" {
		f.add("password", "is required")
	}
	return f.err("the login details are invalid")
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (r refreshRequest) validate() error {
	f := fields{}
	if r.RefreshToken == "" {
		f.add("refresh_token", "is required")
	}
	return f.err("the refresh request is invalid")
}

// userResponse deliberately omits the numeric id. Leaderboards identify people by username, and
// a caller asking about itself is identified by its token, so the internal id never needs to
// leave the server.
type userResponse struct {
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
}

type authResponse struct {
	User   userResponse  `json:"user"`
	Tokens tokenResponse `json:"tokens"`
}

func newUserResponse(user db.User) userResponse {
	return userResponse{
		Username:  user.Username,
		Email:     user.Email,
		CreatedAt: user.CreatedAt,
	}
}

func newTokenResponse(pair auth.TokenPair, now time.Time) tokenResponse {
	return tokenResponse{
		AccessToken:      pair.AccessToken,
		TokenType:        "Bearer",
		ExpiresIn:        int(pair.AccessExpiresAt.Sub(now).Seconds()),
		RefreshToken:     pair.RefreshToken,
		RefreshExpiresIn: int(pair.RefreshExpiresAt.Sub(now).Seconds()),
	}
}

type authHandler struct {
	service *auth.Service
	limiter *ratelimit.Limiter
	cfg     config.Auth
	now     func() time.Time
}

func (h *authHandler) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		WriteError(w, r, err)
		return
	}

	ip := ClientIP(r, h.cfg.TrustProxyIP)
	if err := h.enforce(r, w, "rl:register:ip:"+ip, h.cfg.LoginMaxIP); err != nil {
		WriteError(w, r, err)
		return
	}

	user, pair, err := h.service.Register(r.Context(), auth.Registration{
		Username: req.Username,
		Email:    req.Email,
		Password: req.Password,
	})
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrUsernameTaken):
			WriteError(w, r, Conflict("that username is already registered").
				WithFields(map[string]string{"username": "already registered"}))
		case errors.Is(err, auth.ErrEmailTaken):
			WriteError(w, r, Conflict("that email is already registered").
				WithFields(map[string]string{"email": "already registered"}))
		default:
			WriteError(w, r, err)
		}
		return
	}

	WriteJSON(w, r, http.StatusCreated, authResponse{
		User:   newUserResponse(user),
		Tokens: newTokenResponse(pair, h.now()),
	})
}

func (h *authHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		WriteError(w, r, err)
		return
	}

	// Both limits matter. Per-IP alone lets a botnet grind one account from a thousand
	// addresses; per-account alone lets one address spray a password across a thousand accounts.
	ip := ClientIP(r, h.cfg.TrustProxyIP)
	ipKey := "rl:login:ip:" + ip
	accountKey := "rl:login:account:" + strings.ToLower(req.Login)

	if err := h.enforce(r, w, ipKey, h.cfg.LoginMaxIP); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := h.enforce(r, w, accountKey, h.cfg.LoginMaxAccount); err != nil {
		WriteError(w, r, err)
		return
	}

	user, pair, err := h.service.Login(r.Context(), req.Login, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			WriteError(w, r, Unauthorized("invalid credentials"))
			return
		}
		WriteError(w, r, err)
		return
	}

	// A successful login forgives earlier failures, so a user who mistypes twice and then gets
	// it right is not left with a counter ticking towards a lockout.
	h.reset(r, ipKey, accountKey)

	WriteJSON(w, r, http.StatusOK, authResponse{
		User:   newUserResponse(user),
		Tokens: newTokenResponse(pair, h.now()),
	})
}

func (h *authHandler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		WriteError(w, r, err)
		return
	}

	pair, err := h.service.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrTokenReused):
			observability.Logger(r.Context()).Warn("refresh token reuse detected, family revoked")
			WriteError(w, r, Unauthorized(
				"that refresh token has already been used; the session has been revoked"))
		case errors.Is(err, auth.ErrTokenExpired):
			WriteError(w, r, Unauthorized("that refresh token has expired"))
		case errors.Is(err, auth.ErrInvalidToken):
			WriteError(w, r, Unauthorized("that refresh token is not valid"))
		default:
			WriteError(w, r, err)
		}
		return
	}

	WriteJSON(w, r, http.StatusOK, newTokenResponse(pair, h.now()))
}

func (h *authHandler) handleLogout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		WriteError(w, r, err)
		return
	}

	if err := h.service.Logout(r.Context(), req.RefreshToken); err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, r, http.StatusNoContent, nil)
}

func (h *authHandler) handleMe(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFrom(r.Context())
	if !ok {
		WriteError(w, r, Unauthorized("authentication is required"))
		return
	}

	user, err := h.service.User(r.Context(), userID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			// The token verified but the account is gone: deleted since the token was issued.
			WriteError(w, r, Unauthorized("this account no longer exists"))
			return
		}
		WriteError(w, r, err)
		return
	}

	WriteJSON(w, r, http.StatusOK, newUserResponse(user))
}

func (h *authHandler) enforce(r *http.Request, w http.ResponseWriter, key string, limit int) error {
	return enforceLimit(r, w, h.limiter, key, limit, h.cfg.LoginWindow)
}

// enforceLimit counts one event and returns a 429 error when the caller is over its limit.
func enforceLimit(
	r *http.Request,
	w http.ResponseWriter,
	limiter *ratelimit.Limiter,
	key string,
	limit int,
	window time.Duration,
) error {
	if limiter == nil {
		return nil
	}

	result, err := limiter.Allow(r.Context(), key, limit, window)
	if err != nil {
		// Redis being unavailable must not lock everyone out of their accounts. Log it and let
		// the request through - availability of login matters more than the throttle, and the
		// credential check behind it is still doing its job.
		observability.Logger(r.Context()).Error("rate limit check failed, allowing request",
			slog.Any("error", err))
		return nil
	}

	w.Header().Set("RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("RateLimit-Remaining", strconv.Itoa(result.Remaining))

	if !result.Allowed {
		retryAfter := int(result.RetryAfter.Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		return TooManyRequests("too many attempts; try again later")
	}
	return nil
}

func (h *authHandler) reset(r *http.Request, keys ...string) {
	if h.limiter == nil {
		return
	}
	for _, key := range keys {
		if err := h.limiter.Reset(r.Context(), key); err != nil {
			observability.Logger(r.Context()).Warn("could not reset rate limit counter",
				slog.Any("error", err))
		}
	}
}

// Authenticate rejects requests without a valid access token and puts the user id on the context.
func Authenticate(verifier auth.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				writeUnauthorized(w, r, "a bearer access token is required")
				return
			}

			claims, err := verifier.Verify(raw)
			if err != nil {
				if errors.Is(err, auth.ErrTokenExpired) {
					writeUnauthorized(w, r, "the access token has expired")
					return
				}
				writeUnauthorized(w, r, "the access token is not valid")
				return
			}

			next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), claims.UserID)))
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}

	token = strings.TrimSpace(token)
	return token, token != ""
}

func writeUnauthorized(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="podium"`)
	WriteError(w, r, Unauthorized(message))
}
