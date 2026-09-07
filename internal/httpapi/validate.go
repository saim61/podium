package httpapi

import (
	"net/mail"
	"strings"
	"unicode"
)

const (
	usernameMinLength = 3
	usernameMaxLength = 32
	emailMaxLength    = 254
	passwordMinLength = 12
	passwordMaxLength = 128
)

// fields accumulates per-field validation problems.
type fields map[string]string

func (f fields) add(name, problem string) {
	if _, exists := f[name]; !exists {
		f[name] = problem
	}
}

func (f fields) err(message string) error {
	if len(f) == 0 {
		return nil
	}
	return BadRequest(message).WithFields(f)
}

func validateUsername(f fields, username string) {
	switch {
	case username == "":
		f.add("username", "is required")
		return
	case len(username) < usernameMinLength:
		f.add("username", "must be at least 3 characters")
		return
	case len(username) > usernameMaxLength:
		f.add("username", "must be at most 32 characters")
		return
	}

	for _, r := range username {
		if r > unicode.MaxASCII || (!isASCIILetterOrDigit(r) && r != '_' && r != '-') {
			f.add("username", "may contain only letters, digits, underscores and hyphens")
			return
		}
	}

	if !isASCIILetterOrDigit(rune(username[0])) {
		f.add("username", "must start with a letter or digit")
	}
}

func isASCIILetterOrDigit(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func validateEmail(f fields, email string) {
	switch {
	case email == "":
		f.add("email", "is required")
		return
	case len(email) > emailMaxLength:
		f.add("email", "is too long")
		return
	}

	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email {
		f.add("email", "is not a valid email address")
		return
	}
	if !strings.Contains(strings.SplitN(email, "@", 2)[1], ".") {
		f.add("email", "is not a valid email address")
	}
}

func validatePassword(f fields, password, username string) {
	switch {
	case password == "":
		f.add("password", "is required")
	case len(password) < passwordMinLength:
		f.add("password", "must be at least 12 characters")
	case len(password) > passwordMaxLength:
		// Argon2's cost is independent of input length, but an unbounded body still lets a
		// caller push megabytes through the hasher.
		f.add("password", "must be at most 128 characters")
	case username != "" && strings.EqualFold(password, username):
		f.add("password", "must not be the same as the username")
	}
}
