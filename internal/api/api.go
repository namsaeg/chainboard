// Package api serves the local HTTP API and the simple web UI.
//
// The API controls the wallet, so it listens on localhost and every write
// request must carry the header "X-Chainboard: 1". Browsers cannot add that
// header to a cross-site request without a CORS preflight, which this server
// never approves, so other web pages cannot spend from the wallet.
package api

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/media"
	"github.com/namsaeg/chainboard/internal/node"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/types"
)

//go:embed web
var webFS embed.FS

// Handler builds the HTTP handler for a node.
func Handler(n *node.Node) http.Handler {
	s := &server{n: n}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.status)
	mux.HandleFunc("/api/wallet", s.wallet)
	mux.HandleFunc("/api/boards", s.boards)
	mux.HandleFunc("/api/board", s.board)
	mux.HandleFunc("/api/post", s.post)
	mux.HandleFunc("/api/pending", s.pending)
	mux.HandleFunc("/api/data/", s.data)
	mux.HandleFunc("/api/upload", s.upload)
	mux.HandleFunc("/api/extend", s.extend)
	mux.HandleFunc("/api/like", s.like)
	mux.HandleFunc("/api/stake", s.stake)
	mux.HandleFunc("/api/unstake", s.stake)
	mux.HandleFunc("/api/transfer", s.transfer)
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return guard(mux)
}

func guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Chainboard") != "1" {
				http.Error(w, "missing X-Chainboard header", http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

type server struct{ n *node.Node }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func coins(u uint64) string {
	whole := u / params.Coin
	frac := u % params.Coin
	if frac == 0 {
		return strconv.FormatUint(whole, 10)
	}
	f := strings.TrimRight(strconv.FormatUint(frac+params.Coin, 10)[1:], "0")
	return strconv.FormatUint(whole, 10) + "." + f
}

func parseCoins(s string) (uint64, error) {
	parts := strings.SplitN(strings.TrimSpace(s), ".", 2)
	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, errors.New("bad amount")
	}
	v := whole * params.Coin
	if len(parts) == 2 {
		f := parts[1]
		if len(f) > 8 {
			return 0, errors.New("at most 8 decimals")
		}
		f += strings.Repeat("0", 8-len(f))
		fv, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0, errors.New("bad amount")
		}
		v += fv
	}
	return v, nil
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	n := s.n
	st := n.Chain.Tip()
	writeJSON(w, map[string]any{
		"version":            params.Version,
		"network":            n.P.Name,
		"height":             st.Height,
		"tip":                st.Tip.String(),
		"peers":              n.Host.PeerCount(),
		"p2p_listen":         n.Host.Addr(),
		"storage_price":      coins(st.Price) + " / raw GB-year",
		"base_fee_per_byte":  st.BaseFee,
		"free_lane_pressure": float64(st.Congestion) / 1000,
		"media_cap_block":    st.MediaCapPerBlock(),
		"file_cap":           st.FileCap(),
		"supply":             coins(st.Supply),
		"burned":             coins(st.Burned),
		"endowment":          coins(st.Endowment),
		"repair_pool":        coins(st.RepairPool),
		"proven_bytes":       st.Proven,
		"need_bytes":         st.NeedBytes(),
		"mempool":            n.Pool.Len(),
		"mining":             n.Miner.Stats(),
	})
}

func (s *server) wallet(w http.ResponseWriter, r *http.Request) {
	n := s.n
	addr := n.Wallet.Address()
	st := n.Pool.PendingState()
	acc := st.Peek(addr)
	h := st.Height + 1
	writeJSON(w, map[string]any{
		"address":       addr.String(),
		"balance":       coins(acc.Balance),
		"vesting":       coins(st.Locked(&acc, h)),
		"claimable":     coins(st.Claimable(&acc, h)),
		"stake":         coins(acc.StakeMature + acc.StakePending),
		"stake_pending": coins(acc.StakePending),
		"free_quota":    st.QuotaAt(&acc, h),
		"nonce":         acc.Nonce,
		"queued":        len(n.Queued()),
	})
}

func (s *server) boards(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.n.Index.Boards())
}

func (s *server) board(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minLikes, _ := strconv.ParseUint(q.Get("min_likes"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	writeJSON(w, s.n.Index.Board(q.Get("name"), minLikes, limit))
}

type postReq struct {
	Board  string   `json:"board"`
	Text   string   `json:"text"`
	Parent string   `json:"parent"`
	Lane   string   `json:"lane"`
	Attach []string `json:"attach"`
}

func lane(s string) types.Lane {
	if s == "free" {
		return types.LaneFree
	}
	return types.LaneFee
}

func (s *server) post(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		id, err := cryptox.ParseHash(r.URL.Query().Get("id"))
		if err != nil {
			fail(w, 400, err)
			return
		}
		p, cs := s.n.Index.Thread(id)
		if p == nil {
			fail(w, 404, errors.New("post not found"))
			return
		}
		writeJSON(w, map[string]any{"post": p, "comments": cs})
		return
	}
	var req postReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	if !utf8.ValidString(req.Text) || len(req.Text) > params.MaxPostBytes {
		fail(w, 400, errors.New("text must be UTF-8 and at most 1024 bytes"))
		return
	}
	t := &types.Tx{Kind: types.TxPost, Lane: lane(req.Lane), Board: strings.TrimSpace(req.Board), Text: []byte(req.Text)}
	if req.Parent != "" {
		h, err := cryptox.ParseHash(req.Parent)
		if err != nil {
			fail(w, 400, err)
			return
		}
		t.Parent = h
	}
	for _, a := range req.Attach {
		h, err := cryptox.ParseHash(a)
		if err != nil {
			fail(w, 400, err)
			return
		}
		t.Attach = append(t.Attach, h)
	}
	if err := s.n.Send(t); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]string{"id": t.ID().String()})
}

func (s *server) pending(w http.ResponseWriter, r *http.Request) {
	st := s.n.Chain.Tip()
	var out []node.PostView
	for _, t := range s.n.Queued() {
		if t.Kind == types.TxPost {
			v := node.View(t, 0, 0)
			v.Pending = true
			out = append(out, *v)
		}
	}
	for _, t := range s.n.Pool.Pending() {
		if t.Kind == types.TxPost {
			v := node.View(t, st.Height+1, 0)
			v.Pending = true
			out = append(out, *v)
		}
	}
	writeJSON(w, out)
}

func (s *server) data(w http.ResponseWriter, r *http.Request) {
	root, err := cryptox.ParseHash(strings.TrimPrefix(r.URL.Path, "/api/data/"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	b, err := s.n.Store.Data(root)
	if err != nil {
		fail(w, 404, errors.New("data not stored on this node"))
		return
	}
	_, ct, err := media.Sniff(b)
	if err != nil {
		// Only accepted image types are ever served to the browser.
		fail(w, 415, err)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(b)
}

func tierFrom(s string) (uint16, error) {
	switch s {
	case "perm", "permanent":
		return params.TierPermanent, nil
	case "1m", "month":
		return 1, nil
	}
	y, err := strconv.Atoi(strings.TrimSuffix(s, "y"))
	if err != nil || y < 1 || y > 100 {
		return 0, errors.New("tier must be 1m, 1y..100y or perm")
	}
	return uint16(y * 12), nil
}

func (s *server) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, errors.New("POST only"))
		return
	}
	st := s.n.Chain.Tip()
	r.Body = http.MaxBytesReader(w, r.Body, int64(st.FileCap())*2+1<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		fail(w, 400, err)
		return
	}
	tier, err := tierFrom(r.FormValue("tier"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	res, err := s.n.Upload(raw, tier)
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]any{"root": res.Root.String(), "action": res.Action, "until": res.Until})
}

func (s *server) extend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Root string `json:"root"`
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	root, err := cryptox.ParseHash(req.Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	tier, err := tierFrom(req.Tier)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.n.Send(&types.Tx{Kind: types.TxExtend, DataRoot: root, Tier: tier}); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"queued": true})
}

func (s *server) like(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Lane string `json:"lane"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	id, err := cryptox.ParseHash(req.ID)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.n.Send(&types.Tx{Kind: types.TxLike, Target: id, Lane: lane(req.Lane)}); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"queued": true})
}

func (s *server) stake(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Amount string `json:"amount"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	amt, err := parseCoins(req.Amount)
	if err != nil {
		fail(w, 400, err)
		return
	}
	kind := types.TxStake
	if strings.HasSuffix(r.URL.Path, "unstake") {
		kind = types.TxUnstake
	}
	if err := s.n.Send(&types.Tx{Kind: kind, Amount: amt}); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"queued": true})
}

func (s *server) transfer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To     string `json:"to"`
		Amount string `json:"amount"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	to, err := cryptox.ParseAddress(req.To)
	if err != nil {
		fail(w, 400, err)
		return
	}
	amt, err := parseCoins(req.Amount)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.n.Send(&types.Tx{Kind: types.TxTransfer, To: to, Amount: amt}); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]bool{"queued": true})
}
