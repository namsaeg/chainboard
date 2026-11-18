package api

import (
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/namsaeg/chainboard/internal/node"
	"github.com/namsaeg/chainboard/internal/params"
)

func TestGuard(t *testing.T) {
	p := params.Devnet()
	p.GenesisTarget = new(big.Int).Lsh(big.NewInt(1), 250)
	n, err := node.New(node.Config{Network: "devnet", DataDir: t.TempDir(), Params: p})
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(n)
	req := httptest.NewRequest(http.MethodPost, "/api/post", strings.NewReader(`{"board":"a","text":"b"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("write without X-Chainboard header got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"network":"devnet"`) {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "chainboard") {
		t.Fatal("UI not served")
	}
}
