package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/config"
)

// Deliberately cheap. Production cost is 64 MiB; running that in every test would make the suite
// unusable, and what is under test is the encoding and comparison logic, not Argon2 itself.
func testParams() config.Argon2 {
	return config.Argon2{MemoryKiB: 8192, Iterations: 1, Parallelism: 1}
}

func testHasher(t *testing.T) *Hasher {
	t.Helper()

	h, err := NewHasher(testParams())
	require.NoError(t, err)
	return h
}

func TestHashAndVerify(t *testing.T) {
	h := testHasher(t)

	encoded, err := h.Hash("correct horse battery staple")
	require.NoError(t, err)

	matches, err := Verify(encoded, "correct horse battery staple")
	require.NoError(t, err)
	require.True(t, matches)
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	h := testHasher(t)

	encoded, err := h.Hash("correct horse battery staple")
	require.NoError(t, err)

	matches, err := Verify(encoded, "correct horse battery stapl")
	require.NoError(t, err)
	require.False(t, matches)
}

func TestHashIsSaltedPerCall(t *testing.T) {
	h := testHasher(t)

	first, err := h.Hash("same password")
	require.NoError(t, err)
	second, err := h.Hash("same password")
	require.NoError(t, err)

	require.NotEqual(t, first, second, "identical passwords must not produce identical hashes")

	for _, encoded := range []string{first, second} {
		matches, err := Verify(encoded, "same password")
		require.NoError(t, err)
		require.True(t, matches)
	}
}

func TestHashRecordsItsParameters(t *testing.T) {
	h := testHasher(t)

	encoded, err := h.Hash("whatever")
	require.NoError(t, err)

	require.True(t, strings.HasPrefix(encoded, "$argon2id$v=19$m=8192,t=1,p=1$"), encoded)
	require.Len(t, strings.Split(encoded, "$"), 6)
}

func TestVerifyUsesParametersFromTheStoredHash(t *testing.T) {
	weak, err := NewHasher(config.Argon2{MemoryKiB: 8192, Iterations: 1, Parallelism: 1})
	require.NoError(t, err)

	encoded, err := weak.Hash("legacy password")
	require.NoError(t, err)

	// A hash written at the old cost must keep verifying after the cost is raised, otherwise
	// raising it locks every existing user out.
	matches, err := Verify(encoded, "legacy password")
	require.NoError(t, err)
	require.True(t, matches)
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for name, encoded := range map[string]string{
		"empty":             "",
		"not a hash":        "hunter2",
		"too few fields":    "$argon2id$v=19$m=8192,t=1,p=1$c2FsdA",
		"wrong algorithm":   "$argon2i$v=19$m=8192,t=1,p=1$c2FsdA$aGFzaA",
		"bad version":       "$argon2id$v=18$m=8192,t=1,p=1$c2FsdA$aGFzaA",
		"unparsable cost":   "$argon2id$v=19$m=x,t=1,p=1$c2FsdA$aGFzaA",
		"zero cost":         "$argon2id$v=19$m=0,t=0,p=0$c2FsdA$aGFzaA",
		"bad base64 salt":   "$argon2id$v=19$m=8192,t=1,p=1$!!!$aGFzaA",
		"bad base64 key":    "$argon2id$v=19$m=8192,t=1,p=1$c2FsdA$!!!",
		"empty salt":        "$argon2id$v=19$m=8192,t=1,p=1$$aGFzaA",
		"empty key":         "$argon2id$v=19$m=8192,t=1,p=1$c2FsdA$",
		"no leading dollar": "argon2id$v=19$m=8192,t=1,p=1$c2FsdA$aGFzaA",
	} {
		t.Run(name, func(t *testing.T) {
			matches, err := Verify(encoded, "anything")

			require.Error(t, err)
			require.False(t, matches)
		})
	}
}

func TestNeedsRehash(t *testing.T) {
	current := config.Argon2{MemoryKiB: 16384, Iterations: 2, Parallelism: 2}
	h, err := NewHasher(current)
	require.NoError(t, err)

	atCurrentCost, err := h.Hash("password")
	require.NoError(t, err)
	require.False(t, h.NeedsRehash(atCurrentCost))

	weaker, err := NewHasher(config.Argon2{MemoryKiB: 8192, Iterations: 1, Parallelism: 1})
	require.NoError(t, err)

	atOldCost, err := weaker.Hash("password")
	require.NoError(t, err)
	require.True(t, h.NeedsRehash(atOldCost))

	require.True(t, h.NeedsRehash("garbage"), "an unparsable hash should be replaced")
}

func TestVerifyDecoyDoesNotPanic(t *testing.T) {
	h := testHasher(t)

	require.NotPanics(t, func() { h.VerifyDecoy("anything at all") })
}
