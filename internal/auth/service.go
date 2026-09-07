package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/saim61/podium/internal/config"
	"github.com/saim61/podium/internal/store/db"
)

// Service performs registration, login and refresh token rotation.
type Service struct {
	pool       *pgxpool.Pool
	queries    *db.Queries
	hasher     *Hasher
	tokens     *TokenIssuer
	refreshTTL time.Duration
	now        func() time.Time
}

// Option adjusts a Service at construction.
type Option func(*Service)

// WithClock replaces the time source, so tests can move expiry around without sleeping.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		s.now = now
		s.tokens.now = now
	}
}

// NewService builds the auth service.
func NewService(pool *pgxpool.Pool, cfg config.Auth, opts ...Option) (*Service, error) {
	hasher, err := NewHasher(cfg.Argon2)
	if err != nil {
		return nil, err
	}

	s := &Service{
		pool:       pool,
		queries:    db.New(pool),
		hasher:     hasher,
		tokens:     NewTokenIssuer(cfg),
		refreshTTL: cfg.RefreshTokenTTL,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// TokenPair is what a caller receives on a successful register, login or refresh.
type TokenPair struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

// Registration is a new account request. It is assumed already validated.
type Registration struct {
	Username string
	Email    string
	Password string
}

// Verifier exposes only access token verification, which is all the auth middleware needs.
type Verifier interface {
	Verify(raw string) (AccessClaims, error)
}

// Tokens returns the access token verifier.
func (s *Service) Tokens() *TokenIssuer { return s.tokens }

// Register creates an account and signs the new user in.
func (s *Service) Register(ctx context.Context, r Registration) (db.User, TokenPair, error) {
	hash, err := s.hasher.Hash(r.Password)
	if err != nil {
		return db.User{}, TokenPair{}, err
	}

	user, err := s.queries.CreateUser(ctx, db.CreateUserParams{
		Username:     r.Username,
		Email:        r.Email,
		PasswordHash: hash,
	})
	if err != nil {
		return db.User{}, TokenPair{}, registrationError(err)
	}

	pair, err := s.issuePair(ctx, s.queries, user.ID, uuid.New())
	if err != nil {
		return db.User{}, TokenPair{}, err
	}
	return user, pair, nil
}

func registrationError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.UniqueViolation {
		return fmt.Errorf("create user: %w", err)
	}

	switch pgErr.ConstraintName {
	case "users_username_key":
		return ErrUsernameTaken
	case "users_email_key":
		return ErrEmailTaken
	default:
		return fmt.Errorf("create user: %w", err)
	}
}

// Login authenticates by username or email and signs the user in.
func (s *Service) Login(ctx context.Context, login, password string) (db.User, TokenPair, error) {
	user, err := s.queries.GetUserByLogin(ctx, login)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Spend the same work as a real verification. Returning immediately here would make
			// "no such user" measurably faster than "wrong password", which turns this endpoint
			// into a username oracle.
			s.hasher.VerifyDecoy(password)
			return db.User{}, TokenPair{}, ErrInvalidCredentials
		}
		return db.User{}, TokenPair{}, fmt.Errorf("look up user: %w", err)
	}

	matches, err := Verify(user.PasswordHash, password)
	if err != nil {
		return db.User{}, TokenPair{}, fmt.Errorf("verify password: %w", err)
	}
	if !matches {
		return db.User{}, TokenPair{}, ErrInvalidCredentials
	}

	s.upgradeHashIfStale(ctx, user, password)

	pair, err := s.issuePair(ctx, s.queries, user.ID, uuid.New())
	if err != nil {
		return db.User{}, TokenPair{}, err
	}
	return user, pair, nil
}

// upgradeHashIfStale rehashes a password that was stored at an older cost. It is best effort: the
// login has already succeeded, and failing it because an optimisation did not work would be
// absurd.
func (s *Service) upgradeHashIfStale(ctx context.Context, user db.User, password string) {
	if !s.hasher.NeedsRehash(user.PasswordHash) {
		return
	}

	hash, err := s.hasher.Hash(password)
	if err != nil {
		return
	}
	_ = s.queries.UpdateUserPasswordHash(ctx, db.UpdateUserPasswordHashParams{
		ID:           user.ID,
		PasswordHash: hash,
	})
}

// Refresh exchanges a refresh token for a new pair, rotating the refresh token.
//
// Rotation is what makes theft detectable. Each token is single use, and every token issued from
// one login shares a family id. Presenting a token that has already been used means two parties
// hold it, so the entire family is revoked and both are logged out.
func (s *Service) Refresh(ctx context.Context, rawToken string) (TokenPair, error) {
	hash := hashRefreshToken(rawToken)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenPair{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	claimed, err := q.ClaimRefreshToken(ctx, hash)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return TokenPair{}, fmt.Errorf("claim refresh token: %w", err)
		}
		return TokenPair{}, s.rejectClaim(ctx, tx, q, hash)
	}

	pair, err := s.issuePair(ctx, q, claimed.UserID, claimed.FamilyID)
	if err != nil {
		return TokenPair{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return TokenPair{}, fmt.Errorf("commit refresh: %w", err)
	}
	return pair, nil
}

// rejectClaim explains why a refresh token could not be claimed. The claim is a single
// conditional UPDATE, so it is atomic: of two concurrent refreshes with the same token exactly
// one succeeds, and the loser arrives here and is treated as reuse.
func (s *Service) rejectClaim(ctx context.Context, tx pgx.Tx, q *db.Queries, hash []byte) error {
	existing, err := q.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		}
		return fmt.Errorf("load refresh token: %w", err)
	}

	switch {
	case existing.UsedAt != nil:
		if _, err := q.RevokeRefreshTokenFamily(ctx, existing.FamilyID); err != nil {
			return fmt.Errorf("revoke token family: %w", err)
		}
		// The revocation is the entire point of this branch, so it has to be committed even
		// though the request fails.
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit revocation: %w", err)
		}
		return ErrTokenReused

	case existing.RevokedAt != nil:
		return ErrInvalidToken

	default:
		return ErrTokenExpired
	}
}

// Logout revokes the token family the given refresh token belongs to, ending that session.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	token, err := s.queries.GetRefreshTokenByHash(ctx, hashRefreshToken(rawToken))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// An unknown token is already logged out. Reporting that it was not found would
			// confirm which tokens exist.
			return nil
		}
		return fmt.Errorf("load refresh token: %w", err)
	}

	if _, err := s.queries.RevokeRefreshTokenFamily(ctx, token.FamilyID); err != nil {
		return fmt.Errorf("revoke token family: %w", err)
	}
	return nil
}

// User loads an account by id.
func (s *Service) User(ctx context.Context, userID int64) (db.User, error) {
	user, err := s.queries.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.User{}, ErrUserNotFound
		}
		return db.User{}, fmt.Errorf("load user: %w", err)
	}
	return user, nil
}

func (s *Service) issuePair(ctx context.Context, q *db.Queries, userID int64, family uuid.UUID) (TokenPair, error) {
	access, accessExpiresAt, err := s.tokens.Issue(userID)
	if err != nil {
		return TokenPair{}, err
	}

	raw, hash, err := newRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}

	expiresAt := s.now().Add(s.refreshTTL)
	if _, err := q.CreateRefreshToken(ctx, db.CreateRefreshTokenParams{
		UserID:    userID,
		FamilyID:  family,
		TokenHash: hash,
		ExpiresAt: expiresAt,
	}); err != nil {
		return TokenPair{}, fmt.Errorf("store refresh token: %w", err)
	}

	return TokenPair{
		AccessToken:      access,
		AccessExpiresAt:  accessExpiresAt,
		RefreshToken:     raw,
		RefreshExpiresAt: expiresAt,
	}, nil
}
