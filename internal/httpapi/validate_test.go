package httpapi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateUsername(t *testing.T) {
	for name, tc := range map[string]struct {
		username string
		valid    bool
	}{
		"simple":             {"saeem", true},
		"with digits":        {"saeem61", true},
		"with underscore":    {"saeem_m", true},
		"with hyphen":        {"saeem-m", true},
		"minimum length":     {"abc", true},
		"maximum length":     {strings.Repeat("a", 32), true},
		"empty":              {"", false},
		"too short":          {"ab", false},
		"too long":           {strings.Repeat("a", 33), false},
		"space":              {"saeem m", false},
		"at sign":            {"saeem@m", false},
		"leading hyphen":     {"-saeem", false},
		"leading underscore": {"_saeem", false},
		"non ascii":          {"saeemé", false},
		"emoji":              {"saeem\U0001F600", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := fields{}
			validateUsername(f, tc.username)

			if tc.valid {
				require.Empty(t, f, "%q should be accepted", tc.username)
			} else {
				require.Contains(t, f, "username", "%q should be rejected", tc.username)
			}
		})
	}
}

func TestValidateEmail(t *testing.T) {
	for name, tc := range map[string]struct {
		email string
		valid bool
	}{
		"simple":        {"saeem@example.com", true},
		"subdomain":     {"saeem@mail.example.co.uk", true},
		"plus tag":      {"saeem+podium@example.com", true},
		"empty":         {"", false},
		"no at":         {"saeem.example.com", false},
		"no domain dot": {"saeem@localhost", false},
		"display name":  {"Saeem <saeem@example.com>", false},
		"spaces":        {"sae em@example.com", false},
		"too long":      {strings.Repeat("a", 250) + "@example.com", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := fields{}
			validateEmail(f, tc.email)

			if tc.valid {
				require.Empty(t, f, "%q should be accepted", tc.email)
			} else {
				require.Contains(t, f, "email", "%q should be rejected", tc.email)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	for name, tc := range map[string]struct {
		password string
		username string
		valid    bool
	}{
		"long enough":       {"correct horse battery", "saeem", true},
		"exactly minimum":   {strings.Repeat("a", 12), "saeem", true},
		"exactly maximum":   {strings.Repeat("a", 128), "saeem", true},
		"empty":             {"", "saeem", false},
		"too short":         {"hunter2", "saeem", false},
		"too long":          {strings.Repeat("a", 129), "saeem", false},
		"same as username":  {"loooongusername", "loooongusername", false},
		"username any case": {"LoooongUsername", "loooongusername", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := fields{}
			validatePassword(f, tc.password, tc.username)

			if tc.valid {
				require.Empty(t, f)
			} else {
				require.Contains(t, f, "password")
			}
		})
	}
}

func TestFieldsKeepsFirstProblemPerField(t *testing.T) {
	f := fields{}
	f.add("username", "first")
	f.add("username", "second")

	require.Equal(t, "first", f["username"])
}

func TestFieldsErrIsNilWhenEmpty(t *testing.T) {
	require.NoError(t, fields{}.err("nothing wrong"))
}

func TestRegisterRequestValidation(t *testing.T) {
	err := registerRequest{Username: "a", Email: "nope", Password: "short"}.validate()

	require.Error(t, err)

	var apiErr *Error
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "bad_request", apiErr.Code)
	require.Len(t, apiErr.Fields, 3, "every invalid field should be reported at once")
}

func TestRegisterRequestAcceptsValidInput(t *testing.T) {
	err := registerRequest{
		Username: "saeem",
		Email:    "saeem@example.com",
		Password: "correct horse battery",
	}.validate()

	require.NoError(t, err)
}
