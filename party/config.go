package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// maxSlots is the most player slots a config may define.
const maxSlots = 12

// Config is the content of slots.json.
type Config struct {
	Slots   []SlotConfig   `json:"slots"`
	Sources []SourceConfig `json:"sources"`
}

// SourceConfig is one emulator video stream: the picture of one player (pad) of
// one Dolphin instance, sent as H.264 RTP to 127.0.0.1:Port. partyd forwards a
// source to every slot whose instance and pad match (set by POST /api/nav/assign),
// so the streams never restart when the navigator moves players between instances.
type SourceConfig struct {
	Instance int `json:"instance"`
	Pad      int `json:"pad"`
	Port     int `json:"port"`
}

// SlotConfig describes one player slot. Pipe may be empty: the slot is then
// unassigned and its input is dropped until POST /api/nav/assign gives it a
// FIFO. Instance, Pad and Mode are optional and only reported to the navigator
// (GET /api/nav/lobby).
type SlotConfig struct {
	Slot     int    `json:"slot"`
	RTPPort  int    `json:"rtpPort"`
	Pipe     string `json:"pipe"`
	Label    string `json:"label"`
	Instance int    `json:"instance,omitempty"`
	Pad      int    `json:"pad,omitempty"`
	Mode     string `json:"mode,omitempty"`
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(cfg.Slots) == 0 {
		return nil, fmt.Errorf("%s: no slots defined", path)
	}
	if len(cfg.Slots) > maxSlots {
		return nil, fmt.Errorf("%s: %d slots defined, at most %d are allowed", path, len(cfg.Slots), maxSlots)
	}
	seenSlot := map[int]bool{}
	seenPort := map[int]bool{}
	seenPipe := map[string]bool{}
	for i := range cfg.Slots {
		s := &cfg.Slots[i]
		if s.Slot <= 0 {
			return nil, fmt.Errorf("slot numbers must be >= 1 (got %d)", s.Slot)
		}
		if seenSlot[s.Slot] {
			return nil, fmt.Errorf("duplicate slot %d", s.Slot)
		}
		if seenPort[s.RTPPort] {
			return nil, fmt.Errorf("duplicate rtpPort %d", s.RTPPort)
		}
		if s.RTPPort <= 0 || s.RTPPort > 65535 {
			return nil, fmt.Errorf("slot %d: bad rtpPort %d", s.Slot, s.RTPPort)
		}
		if s.Pipe != "" {
			abs, err := filepath.Abs(s.Pipe)
			if err != nil {
				return nil, err
			}
			if seenPipe[abs] {
				return nil, fmt.Errorf("slot %d: pipe %s is used by another slot", s.Slot, abs)
			}
			seenPipe[abs] = true
			s.Pipe = abs
		}
		if s.Label == "" {
			s.Label = fmt.Sprintf("P%d", s.Slot)
		}
		seenSlot[s.Slot] = true
		seenPort[s.RTPPort] = true
	}
	seenSrc := map[[2]int]bool{}
	for _, src := range cfg.Sources {
		if src.Instance <= 0 || src.Pad <= 0 {
			return nil, fmt.Errorf("source: instance and pad must be >= 1 (got %d/%d)", src.Instance, src.Pad)
		}
		if src.Port <= 0 || src.Port > 65535 {
			return nil, fmt.Errorf("source i%d pad %d: bad port %d", src.Instance, src.Pad, src.Port)
		}
		if seenPort[src.Port] {
			return nil, fmt.Errorf("source i%d pad %d: port %d is used twice", src.Instance, src.Pad, src.Port)
		}
		key := [2]int{src.Instance, src.Pad}
		if seenSrc[key] {
			return nil, fmt.Errorf("duplicate source instance %d pad %d", src.Instance, src.Pad)
		}
		seenSrc[key] = true
		seenPort[src.Port] = true
	}
	return &cfg, nil
}
