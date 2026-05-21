package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wallet-transfer-service/internal/handler"
)

// =============================================================================
// HTTP integration: idempotency key as a header, not in the body
// =============================================================================

// httpEnv wraps testEnv with a chi-routed HTTP server backed by the same DB.
type httpEnv struct {
	env    *testEnv
	server *httptest.Server
}

func newHTTPEnv(t *testing.T) *httpEnv {
	t.Helper()
	env := newTestEnv(t)
	transferHandler := handler.NewTransferHandler(env.svc)
	walletHandler := handler.NewWalletHandler(env.svc)
	router := handler.NewRouter(transferHandler, walletHandler)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return &httpEnv{env: env, server: server}
}

func (h *httpEnv) postTransfer(idempotencyKey string, body any) *http.Response {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		h.server.URL+"/transfers", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set(handler.IdempotencyKeyHeader, idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

func readBody(resp *http.Response) map[string]any {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func TestHTTP_HappyPath_KeyInHeader(t *testing.T) {
	env := newHTTPEnv(t)
	env.env.seedWallet("w1", 500)
	env.env.seedWallet("w2", 0)

	resp := env.postTransfer("key-http-1", map[string]any{
		"fromWalletId": "w1",
		"toWalletId":   "w2",
		"amount":       100,
	})
	body := readBody(resp)

	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "PROCESSED", body["status"])
	assert.Equal(t, "w1", body["fromWalletId"])
	assert.Equal(t, "w2", body["toWalletId"])
	assert.Equal(t, float64(100), body["amount"]) // JSON numbers are float64

	assert.Equal(t, int64(400), env.env.walletBalance("w1"))
	assert.Equal(t, int64(100), env.env.walletBalance("w2"))
}

func TestHTTP_MissingHeader_Returns400(t *testing.T) {
	env := newHTTPEnv(t)
	env.env.seedWallet("w1", 500)
	env.env.seedWallet("w2", 0)

	resp := env.postTransfer("", map[string]any{
		"fromWalletId": "w1",
		"toWalletId":   "w2",
		"amount":       100,
	})
	body := readBody(resp)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "MISSING_IDEMPOTENCY_KEY", body["error"])

	// No transfer row created.
	assert.Equal(t, 0, env.env.countTransfers())
}

func TestHTTP_KeyInBodyIsRejected(t *testing.T) {
	// The body now uses {fromWalletId, toWalletId, amount}. If a client sends
	// an old-style payload with idempotencyKey in the body AND no header, they
	// should get a 400 (unknown field + missing header). This guards against
	// silent regressions where someone re-adds key-in-body.
	env := newHTTPEnv(t)
	env.env.seedWallet("w1", 500)
	env.env.seedWallet("w2", 0)

	// No header AT ALL but key in body — the header check fires first.
	resp := env.postTransfer("", map[string]any{
		"idempotencyKey": "key-x",
		"fromWalletId":   "w1",
		"toWalletId":     "w2",
		"amount":         100,
	})
	body := readBody(resp)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "MISSING_IDEMPOTENCY_KEY", body["error"])

	// With header set + unknown field in body, DisallowUnknownFields rejects.
	resp = env.postTransfer("key-x", map[string]any{
		"idempotencyKey": "key-x",
		"fromWalletId":   "w1",
		"toWalletId":     "w2",
		"amount":         100,
	})
	body = readBody(resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "VALIDATION", body["error"])
}

func TestHTTP_ReplaySameKeyHeader(t *testing.T) {
	env := newHTTPEnv(t)
	env.env.seedWallet("w1", 500)
	env.env.seedWallet("w2", 0)

	body := map[string]any{
		"fromWalletId": "w1",
		"toWalletId":   "w2",
		"amount":       100,
	}

	// First call.
	resp1 := env.postTransfer("rk-1", body)
	b1 := readBody(resp1)
	require.Equal(t, http.StatusCreated, resp1.StatusCode)

	// Retry with the same header + body.
	resp2 := env.postTransfer("rk-1", body)
	b2 := readBody(resp2)
	require.Equal(t, http.StatusCreated, resp2.StatusCode)

	// Same transfer ID returned.
	assert.Equal(t, b1["id"], b2["id"])

	// Money moved exactly once.
	assert.Equal(t, int64(400), env.env.walletBalance("w1"))
	assert.Equal(t, int64(100), env.env.walletBalance("w2"))
	assert.Equal(t, 1, env.env.countTransfers())
	assert.Equal(t, 2, env.env.countLedgerEntries())
}

func TestHTTP_ReplaySameKey_DifferentBody_Returns409(t *testing.T) {
	env := newHTTPEnv(t)
	env.env.seedWallet("w1", 500)
	env.env.seedWallet("w2", 0)
	env.env.seedWallet("w3", 0)

	// First call.
	resp1 := env.postTransfer("rk-2", map[string]any{
		"fromWalletId": "w1",
		"toWalletId":   "w2",
		"amount":       100,
	})
	require.Equal(t, http.StatusCreated, resp1.StatusCode)

	// Second call with same key but different destination.
	resp2 := env.postTransfer("rk-2", map[string]any{
		"fromWalletId": "w1",
		"toWalletId":   "w3",
		"amount":       100,
	})
	body := readBody(resp2)
	assert.Equal(t, http.StatusConflict, resp2.StatusCode)
	assert.Equal(t, "IDEMPOTENCY_KEY_REUSE", body["error"])
}

func TestHTTP_GetWallet(t *testing.T) {
	env := newHTTPEnv(t)
	env.env.seedWallet("w1", 250)

	resp, err := http.Get(env.server.URL + "/wallets/w1")
	require.NoError(t, err)
	body := readBody(resp)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "w1", body["id"])
	assert.Equal(t, float64(250), body["balance"])
}

func TestHTTP_GetWallet_NotFound(t *testing.T) {
	env := newHTTPEnv(t)
	resp, err := http.Get(env.server.URL + "/wallets/missing")
	require.NoError(t, err)
	body := readBody(resp)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "WALLET_NOT_FOUND", body["error"])
}

func TestHTTP_ListWalletTransfers(t *testing.T) {
	env := newHTTPEnv(t)
	env.env.seedWallet("w1", 500)
	env.env.seedWallet("w2", 0)

	// Make two transfers.
	resp := env.postTransfer("h-1", map[string]any{
		"fromWalletId": "w1", "toWalletId": "w2", "amount": 100,
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp = env.postTransfer("h-2", map[string]any{
		"fromWalletId": "w1", "toWalletId": "w2", "amount": 50,
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp, err := http.Get(env.server.URL + "/wallets/w1/transfers?limit=10")
	require.NoError(t, err)
	body := readBody(resp)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "w1", body["walletId"])
	assert.Equal(t, float64(2), body["count"])
	transfers, ok := body["transfers"].([]any)
	require.True(t, ok)
	require.Len(t, transfers, 2)
}
