package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"strings"
)

const maxBody = 1 << 20

type joinReq struct {
	ClientID string `json:"clientId"`
	WantSlot int    `json:"wantSlot"`
	SDP      string `json:"sdp"`
}

type joinResp struct {
	Slot int    `json:"slot"`
	SDP  string `json:"sdp"`
}

type watchReq struct {
	Slot int    `json:"slot"`
	SDP  string `json:"sdp"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handler(web fs.FS) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/join", func(w http.ResponseWriter, r *http.Request) {
		var req joinReq
		if !decode(w, r, &req) {
			return
		}
		if req.ClientID == "" || len(req.ClientID) > 128 {
			writeErr(w, http.StatusBadRequest, "clientId required (max 128 chars)")
			return
		}
		if !strings.HasPrefix(req.SDP, "v=0") {
			writeErr(w, http.StatusBadRequest, "sdp must be an SDP offer")
			return
		}
		slotNum, answer, err := s.join(req.ClientID, req.WantSlot, req.SDP)
		switch {
		case errors.Is(err, errNoSlot):
			writeErr(w, http.StatusConflict, "no free slot")
		case errors.Is(err, errSuperseded), errors.Is(err, errGameInProgress):
			writeErr(w, http.StatusConflict, err.Error())
		case err != nil:
			log.Printf("join failed: %v", err)
			writeErr(w, http.StatusBadRequest, "could not negotiate: "+err.Error())
		default:
			writeJSON(w, http.StatusOK, joinResp{Slot: slotNum, SDP: answer})
		}
	})

	mux.HandleFunc("/api/watch", func(w http.ResponseWriter, r *http.Request) {
		var req watchReq
		if !decode(w, r, &req) {
			return
		}
		if !strings.HasPrefix(req.SDP, "v=0") {
			writeErr(w, http.StatusBadRequest, "sdp must be an SDP offer")
			return
		}
		answer, err := s.watch(req.Slot, req.SDP)
		switch {
		case errors.Is(err, errUnknownSlot):
			writeErr(w, http.StatusNotFound, "unknown slot")
		case err != nil:
			writeErr(w, http.StatusBadRequest, "could not negotiate: "+err.Error())
		default:
			writeJSON(w, http.StatusOK, joinResp{Slot: req.Slot, SDP: answer})
		}
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"slots": s.state()})
	})

	// Lobby (phones and TV).
	mux.HandleFunc("/api/lobby", s.handleLobbyGet(false))
	mux.HandleFunc("/api/lobby/player", s.handleLobbyPlayer)
	mux.HandleFunc("/api/lobby/start", s.handleLobbyStart)
	mux.HandleFunc("/api/lobby/reset", s.handleLobbyReset)

	// Navigator (loopback only; each handler checks).
	mux.HandleFunc("/api/nav/lobby", s.handleNavLobby)
	mux.HandleFunc("/api/nav/update", s.handleNavUpdate)
	mux.HandleFunc("/api/nav/reset", s.handleNavReset)
	mux.HandleFunc("/api/nav/fake", s.handleNavFake)
	mux.HandleFunc("/api/nav/assign", s.handleNavAssign)

	// How to join: phone URLs and QR codes for them.
	mux.HandleFunc("/api/joininfo", s.handleJoinInfo)
	mux.HandleFunc("/api/joinqr.png", s.handleJoinQR)

	// Phone network statistics.
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.handleStatsPost(w, r)
			return
		}
		s.handleStatsGet(w, r)
	})

	mux.Handle("/", http.FileServerFS(web))
	return noStore(mux)
}

// noStore stops phones from running stale JS after a server restart.
func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}
