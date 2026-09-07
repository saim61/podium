package auth

import "errors"

// Domain failures. The transport layer maps these to status codes; nothing here knows about HTTP.
var (
	// ErrInvalidCredentials covers both an unknown account and a wrong password. They are one
	// error on purpose: distinguishing them tells an attacker which usernames exist.
	ErrInvalidCredentials = errors.New("invalid credentials")

	ErrUsernameTaken = errors.New("username is already registered")
	ErrEmailTaken    = errors.New("email is already registered")

	ErrInvalidToken = errors.New("token is invalid")
	ErrTokenExpired = errors.New("token has expired")

	// ErrTokenReused means a refresh token was presented twice. The whole token family has been
	// revoked by the time this is returned.
	ErrTokenReused = errors.New("refresh token has already been used")

	ErrUserNotFound = errors.New("user not found")
)
