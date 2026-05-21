package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// CanonicalPayloadHash computes the SHA-256 hash of the canonical JSON
// representation of a transfer's payload-identity fields.
//
// Canonicalization rules (§9 of DESIGN.md):
//   - Keys sorted lexicographically (amount, fromWalletId, toWalletId).
//   - No whitespace.
//   - Integers serialized as digit strings (no decimal point, no exponent).
//   - Absent fields omitted (no `null`).
//   - UTF-8 encoding.
//
// CRITICAL: the idempotency_key itself is NOT part of the hash. The key is
// the *index* into the cache of outcomes; the hash is the *payload identity*
// check. Same key + same hash = legitimate retry; same key + different hash
// = client bug, return 4xx.
func CanonicalPayloadHash(fromWalletID, toWalletID string, amount int64) string {
	// Hand-rolled canonical JSON. Keys are emitted in lexicographic order
	// (amount, fromWalletId, toWalletId). amount is serialized as a digit
	// string per the spec; wallet IDs use Go's %q which produces JSON-safe
	// double-quoted UTF-8.
	canonical := fmt.Sprintf(
		`{"amount":"%d","fromWalletId":%q,"toWalletId":%q}`,
		amount, fromWalletID, toWalletID,
	)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}
