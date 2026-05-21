package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/wallet-transfer-service/internal/service"
)

func TestCanonicalPayloadHash_Deterministic(t *testing.T) {
	// Same inputs → same hash, always.
	h1 := service.CanonicalPayloadHash("w1", "w2", 100)
	h2 := service.CanonicalPayloadHash("w1", "w2", 100)
	assert.Equal(t, h1, h2)
	assert.Len(t, h1, 64) // SHA-256 hex
}

func TestCanonicalPayloadHash_DifferentAmount(t *testing.T) {
	h1 := service.CanonicalPayloadHash("w1", "w2", 100)
	h2 := service.CanonicalPayloadHash("w1", "w2", 200)
	assert.NotEqual(t, h1, h2)
}

func TestCanonicalPayloadHash_DifferentWallets(t *testing.T) {
	h1 := service.CanonicalPayloadHash("w1", "w2", 100)
	h2 := service.CanonicalPayloadHash("w1", "w3", 100)
	assert.NotEqual(t, h1, h2)
}

func TestCanonicalPayloadHash_OrderMattersForFromTo(t *testing.T) {
	// A→B (100) is a different transfer than B→A (100), so hashes must differ.
	h1 := service.CanonicalPayloadHash("w1", "w2", 100)
	h2 := service.CanonicalPayloadHash("w2", "w1", 100)
	assert.NotEqual(t, h1, h2)
}

func TestCanonicalPayloadHash_KeyExcluded(t *testing.T) {
	// The idempotency key is NOT part of the hash — same payload should
	// hash the same regardless of which key is used to identify it.
	// (Implicit: the function doesn't take a key, so this is structurally
	// enforced. Test documents the intent.)
	h := service.CanonicalPayloadHash("w1", "w2", 100)
	assert.Len(t, h, 64)
}
