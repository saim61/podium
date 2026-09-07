package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/saim61/podium/internal/config"
)

const (
	saltLength = 16
	keyLength  = 32
)

// ErrInvalidHash reports a stored hash that cannot be parsed.
var ErrInvalidHash = errors.New("password hash is malformed")

// Hasher produces Argon2id password hashes at a configured cost.
type Hasher struct {
	params config.Argon2
	decoy  string
}

// NewHasher builds a hasher and computes the decoy hash used to equalise login timing.
func NewHasher(params config.Argon2) (*Hasher, error) {
	h := &Hasher{params: params}

	decoy, err := h.Hash("podium-timing-equaliser")
	if err != nil {
		return nil, fmt.Errorf("build decoy hash: %w", err)
	}
	h.decoy = decoy

	return h, nil
}

// Hash returns an encoded Argon2id hash carrying its own salt and cost parameters.
func (h *Hasher) Hash(password string) (string, error) {
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	key := argon2.IDKey(
		[]byte(password), salt,
		h.params.Iterations, h.params.MemoryKiB, h.params.Parallelism, keyLength,
	)

	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.params.MemoryKiB, h.params.Iterations, h.params.Parallelism,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyDecoy performs a hash comparison that cannot succeed. It is called when a login names an
// account that does not exist, so that response time does not reveal which usernames are real.
func (h *Hasher) VerifyDecoy(password string) {
	_, _ = Verify(h.decoy, password)
}

// NeedsRehash reports whether a stored hash was produced at a different cost than the current
// configuration, so a password can be upgraded on the next successful login.
func (h *Hasher) NeedsRehash(encoded string) bool {
	params, _, _, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return params != h.params
}

// Verify reports whether password matches the encoded hash. Cost parameters are read from the
// hash itself, so hashes written at an older cost keep verifying.
func Verify(encoded, password string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey(
		[]byte(password), salt,
		params.Iterations, params.MemoryKiB, params.Parallelism, uint32(len(want)),
	)

	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func decodeHash(encoded string) (config.Argon2, []byte, []byte, error) {
	var zero config.Argon2

	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return zero, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return zero, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return zero, nil, nil, fmt.Errorf("%w: unsupported argon2 version %d", ErrInvalidHash, version)
	}

	var params config.Argon2
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&params.MemoryKiB, &params.Iterations, &params.Parallelism); err != nil {
		return zero, nil, nil, ErrInvalidHash
	}
	if params.Iterations == 0 || params.MemoryKiB == 0 || params.Parallelism == 0 {
		return zero, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return zero, nil, nil, ErrInvalidHash
	}

	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return zero, nil, nil, ErrInvalidHash
	}

	return params, salt, key, nil
}
