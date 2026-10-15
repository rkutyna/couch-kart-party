package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// slotCounters are server-side numbers for one slot: RTP in from ffmpeg,
// packets written to the WebRTC track, and RTCP feedback from the phone.
type slotCounters struct {
	rtpPkts     atomic.Uint64
	rtpBytes    atomic.Uint64
	trackWrites atomic.Uint64
	trackErrs   atomic.Uint64
	pli         atomic.Uint64
	fir         atomic.Uint64
	nack        atomic.Uint64

	// Rolled up once a second by runRollup. lastPkts and lastBytes are only
	// touched by that goroutine.
	pktsSec   atomic.Uint64
	bytesSec  atomic.Uint64
	lastPkts  uint64
	lastBytes uint64
}

type counterView struct {
	RTPPacketsTotal   uint64 `json:"rtpPacketsTotal"`
	RTPBytesTotal     uint64 `json:"rtpBytesTotal"`
	RTPPacketsLastSec uint64 `json:"rtpPacketsLastSec"`
	RTPBytesLastSec   uint64 `json:"rtpBytesLastSec"`
	TrackWrites       uint64 `json:"trackWrites"`
	TrackWriteErrors  uint64 `json:"trackWriteErrors"`
	PLI               uint64 `json:"pli"`
	FIR               uint64 `json:"fir"`
	NACK              uint64 `json:"nack"`
}

func (c *slotCounters) view() counterView {
	return counterView{
		RTPPacketsTotal:   c.rtpPkts.Load(),
		RTPBytesTotal:     c.rtpBytes.Load(),
		RTPPacketsLastSec: c.pktsSec.Load(),
		RTPBytesLastSec:   c.bytesSec.Load(),
		TrackWrites:       c.trackWrites.Load(),
		TrackWriteErrors:  c.trackErrs.Load(),
		PLI:               c.pli.Load(),
		FIR:               c.fir.Load(),
		NACK:              c.nack.Load(),
	}
}

// runRollup computes the last-second RTP counters. It runs until stop closes.
// Slots are fixed after startup, so reading s.slots here needs no lock.
func (s *Server) runRollup(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			for _, sl := range s.slots {
				c := sl.ctr
				p, b := c.rtpPkts.Load(), c.rtpBytes.Load()
				c.pktsSec.Store(p - c.lastPkts)
				c.bytesSec.Store(b - c.lastBytes)
				c.lastPkts, c.lastBytes = p, b
			}
		}
	}
}

// statsLog appends one JSON object per line to phonestats-<start>.jsonl.
type statsLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

func openStatsLog(dir string, started time.Time) (*statsLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "phonestats-"+started.Format("20060102-150405")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &statsLog{f: f, path: path}, nil
}

func (l *statsLog) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("stats log: %v", err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		log.Printf("stats log write: %v", err)
	}
}

func (l *statsLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.f.Close()
}

// statsLine is one line of the phonestats JSONL file.
type statsLine struct {
	ServerMs   int64          `json:"serverMs"`
	ServerTime string         `json:"serverTime"`
	Slot       int            `json:"slot"`
	Stats      map[string]any `json:"stats"`
	Server     counterView    `json:"server"`
}

// handleStatsPost takes one stats object from a phone. The clientId must hold
// a slot. The object is stored as the slot's latest and appended to the log.
func (s *Server) handleStatsPost(w http.ResponseWriter, r *http.Request) {
	var m map[string]any
	if !decode(w, r, &m) {
		return
	}
	cid, _ := m["clientId"].(string)
	delete(m, "clientId")
	now := time.Now()
	s.mu.Lock()
	sl := s.ownerSlotLocked(cid)
	if sl == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusForbidden, "this phone does not hold a player slot")
		return
	}
	sl.latest = m
	sl.latestAt = now
	line := statsLine{
		ServerMs:   now.UnixMilli(),
		ServerTime: now.UTC().Format(time.RFC3339Nano),
		Slot:       sl.cfg.Slot,
		Stats:      m,
		Server:     sl.ctr.view(),
	}
	s.mu.Unlock()
	if s.statsLog != nil {
		s.statsLog.write(line)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type slotStatsView struct {
	Slot      int            `json:"slot"`
	Label     string         `json:"label"`
	Occupied  bool           `json:"occupied"`
	Connected bool           `json:"connected"`
	StatsAt   int64          `json:"statsAtMs,omitempty"`
	Stats     map[string]any `json:"stats"`
	Server    counterView    `json:"server"`
}

// handleStatsGet returns the latest stats object and the server counters for
// every slot.
func (s *Server) handleStatsGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	s.mu.Lock()
	out := make([]slotStatsView, 0, len(s.slots))
	for _, sl := range s.slots {
		v := slotStatsView{
			Slot:      sl.cfg.Slot,
			Label:     sl.cfg.Label,
			Occupied:  sl.owner != "",
			Connected: sl.connected,
			Stats:     sl.latest,
			Server:    sl.ctr.view(),
		}
		if !sl.latestAt.IsZero() {
			v.StatsAt = sl.latestAt.UnixMilli()
		}
		out = append(out, v)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"slots": out})
}
