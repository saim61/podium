package games

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// NewSeed returns a seed for a new session.
func NewSeed() (int64, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, fmt.Errorf("generate session seed: %w", err)
	}
	return int64(binary.BigEndian.Uint64(buf[:]) >> 1), nil
}

// derive returns a deterministic value for a (seed, label, index) triple.
//
// Hashing rather than a sequential PRNG, so any value can be recomputed on its own without
// having generated the ones before it. That is what makes a finished session auditable: given
// only the stored seed, the nth problem, word or sequence can be regenerated to check what the
// player was actually asked.
func derive(seed int64, label string, index int) uint64 {
	h := sha256.New()

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(seed))
	h.Write(buf[:])
	h.Write([]byte(label))
	binary.BigEndian.PutUint64(buf[:], uint64(index))
	h.Write(buf[:])

	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}

// deriveIntn returns a deterministic value in [0, n).
func deriveIntn(seed int64, label string, index, n int) int {
	if n <= 0 {
		return 0
	}
	return int(derive(seed, label, index) % uint64(n))
}

// deriveRange returns a deterministic value in [low, high].
func deriveRange(seed int64, label string, index, low, high int) int {
	if high <= low {
		return low
	}
	return low + deriveIntn(seed, label, index, high-low+1)
}
