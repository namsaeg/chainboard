package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/namsaeg/chainboard/internal/cryptox"
)

// bounties lists bounties on chain, open first.
func (s *server) bounties(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	writeJSON(w, s.n.BountyList(limit))
}

// bountyNew locks coins for wanted data: {board, note, amount, days, tier,
// refs: [data roots already stored], confirm}.
func (s *server) bountyNew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Board   string   `json:"board"`
		Note    string   `json:"note"`
		Amount  string   `json:"amount"`
		Days    uint64   `json:"days"`
		Tier    string   `json:"tier"`
		Refs    []string `json:"refs"`
		Confirm bool     `json:"confirm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	amt, err := parseCoins(req.Amount)
	if err != nil {
		fail(w, 400, err)
		return
	}
	tier, err := tierFrom(req.Tier)
	if err != nil {
		fail(w, 400, err)
		return
	}
	var refs []cryptox.Hash
	for _, x := range req.Refs {
		h, err := cryptox.ParseHash(strings.TrimSpace(x))
		if err != nil {
			fail(w, 400, err)
			return
		}
		refs = append(refs, h)
	}
	id, err := s.n.CreateBounty(strings.TrimSpace(req.Board), req.Note, refs, amt, req.Days, tier, req.Confirm)
	if err != nil {
		sendFail(w, err)
		return
	}
	writeJSON(w, map[string]string{"id": id.String()})
}

// bountyOffers lists the offers known for a bounty (?id=).
func (s *server) bountyOffers(w http.ResponseWriter, r *http.Request) {
	id, err := cryptox.ParseHash(r.URL.Query().Get("id"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, s.n.BountyOffers(id))
}

// bountyOffer answers a bounty: multipart bounty, file, optional preview
// (a free-form picture), note. The file stays on this node until accepted.
func (s *server) bountyOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, errors.New("POST only"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.n.Chain.Tip().FileCap())*2+4<<20)
	id, err := cryptox.ParseHash(r.FormValue("bounty"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	read := func(name string) ([]byte, error) {
		f, _, err := r.FormFile(name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(f)
	}
	raw, err := read("file")
	if err != nil {
		fail(w, 400, err)
		return
	}
	preview, _ := read("preview")
	o, err := s.n.CreateOffer(id, raw, preview, r.FormValue("note"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, map[string]string{"id": o.ID().String()})
}

// bountyPreview fetches an offer's previews (?offer=): the free one, and
// the data's leading chunks proven against its root.
func (s *server) bountyPreview(w http.ResponseWriter, r *http.Request) {
	id, err := cryptox.ParseHash(r.URL.Query().Get("offer"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	pv, err := s.n.PreviewOffer(id)
	if err != nil {
		fail(w, 404, err)
		return
	}
	writeJSON(w, pv)
}

func (s *server) bountyAccept(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Offer   string `json:"offer"`
		Confirm bool   `json:"confirm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	id, err := cryptox.ParseHash(req.Offer)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.n.AcceptOffer(id, req.Confirm); err != nil {
		sendFail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"queued": true})
}
