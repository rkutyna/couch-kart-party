package main

// A very small "driver" for the fake phones, so that a race can actually be
// finished in tests: it reads the navigator's copy of the slot's game frame
// (run/frames/i<slot>.bmp, written by run/stream_slot.sh about 12 times a
// second), finds the grey road in a band ahead of the kart and steers towards
// its middle with A held. When the picture stops changing (stuck on a wall) it
// reverses for a moment. A is released briefly every 2.5 s so that it also
// works as a fresh press on the results screens.

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"time"
)

var framesDir = filepathJoin("..", "run", "frames")

func filepathJoin(a ...string) string { return filepath.Join(a...) }

type driver struct {
	steer      float64
	lastSeen   float64 // side the road was last seen on (-1 left, +1 right)
	prev       []float64
	stillSince time.Time
	reverseTil time.Time
	revSteer   float64
	lastMtime  time.Time
	offSince   time.Time
	mm         mmState
}

// view is the part of an instance frame (run/frames/i<k>.bmp) that one phone sees: the
// 16:9 picture of a single-player instance (frame rows 186..886), or the top (pad 1) /
// bottom (pad 2) half of a two-player split screen (frame rows 56..536 / 536..1016).
type view struct {
	path string
	two  bool
	top  int     // frame row of the view's first row
	ys   float64 // view height / 700 (the single-player picture height)
}

func viewFor(inst, pad int, mode string) view {
	p := filepath.Join(framesDir, "i"+itoa(inst)+".bmp")
	if mode == "2p" {
		top := 56
		if pad == 2 {
			top = 536
		}
		return view{path: p, two: true, top: top, ys: 480.0 / 700.0}
	}
	return view{path: p, top: 186, ys: 1}
}

// readBand reads the band rows [y0,y1) of the picture (frame rows from `top`) from a
// 24-bit BMP, sampling every 4th pixel. Returns per-sample (r,g,b) rows.
func readBand(path string, top, y0, y1 int) (w int, rows [][][3]uint8, mtime time.Time, ok bool) {
	st, err := os.Stat(path)
	if err != nil || time.Since(st.ModTime()) > 2*time.Second {
		return 0, nil, time.Time{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 54 || b[0] != 'B' || b[1] != 'M' {
		return 0, nil, time.Time{}, false
	}
	off := int(binary.LittleEndian.Uint32(b[10:]))
	width := int(int32(binary.LittleEndian.Uint32(b[18:])))
	height := int(int32(binary.LittleEndian.Uint32(b[22:])))
	bpp := int(binary.LittleEndian.Uint16(b[28:]))
	if bpp != 24 || width <= 0 {
		return 0, nil, time.Time{}, false
	}
	bottomUp := height > 0
	if height < 0 {
		height = -height
	}
	stride := (width*3 + 3) &^ 3
	for y := y0; y < y1; y += 4 {
		fy := top + y
		if fy >= height {
			break
		}
		ry := fy
		if bottomUp {
			ry = height - 1 - fy
		}
		base := off + ry*stride
		if base+width*3 > len(b) {
			return 0, nil, time.Time{}, false
		}
		var row [][3]uint8
		for x := 0; x < width; x += 4 {
			p := base + x*3
			row = append(row, [3]uint8{b[p+2], b[p+1], b[p]})
		}
		rows = append(rows, row)
	}
	return width, rows, st.ModTime(), true
}

// isRoad: grey asphalt (low saturation, mid brightness).
func isRoad(px [3]uint8) bool {
	r, g, b := float64(px[0]), float64(px[1]), float64(px[2])
	mx := math.Max(r, math.Max(g, b))
	mn := math.Min(r, math.Min(g, b))
	lum := (r + g + b) / 3
	return mx-mn < 28 && lum > 55 && lum < 150
}

// rowTarget finds the road segments of one sampled row (every 4th pixel) and returns
// the centre of the segment nearest to the screen centre (where the kart is), its width
// in samples, and whether one was found. Segments shorter than 8 samples are noise.
func rowTarget(row [][3]uint8, w int) (float64, int, bool) {
	best, bestW, bestD := 0.0, 0, math.MaxFloat64
	n := len(row)
	limit := n * 82 / 100 // minimap on the right
	i := 0
	for i < limit {
		if !isRoad(row[i]) {
			i++
			continue
		}
		j := i
		gap := 0
		for j < limit && (isRoad(row[j]) || gap < 2) {
			if isRoad(row[j]) {
				gap = 0
			} else {
				gap++
			}
			j++
		}
		if j-i >= 8 {
			l, r := float64(i*4), float64(j*4)
			c := float64(w) / 2
			d := 0.0
			if c < l {
				d = l - c
			} else if c > r {
				d = c - r
			}
			if d < bestD {
				bestD, best, bestW = d, (l+r)/2, j-i
			}
		}
		i = j + 1
	}
	return best, bestW, bestW > 0
}

// next returns stick x and buttons for this slot's kart. The road (grey asphalt) is
// looked for in a window around the kart (x 380..900 of 1280: the minimap on the
// right is grey too): a near band below the horizon without the kart itself, and a far
// band for the bends. Steer towards the road's centre; with too little road in view
// (grass, a wall) back up for a moment.
func (d *driver) next(v view, now time.Time) (float64, int) {
	a := 1
	if math.Mod(float64(now.UnixMilli())/1000.0, 2.5) > 2.35 {
		a = 0
	}
	if now.Before(d.reverseTil) {
		return d.revSteer, 2 // B: brake / reverse
	}
	if x, btn, ok := d.mm.pursue(v.path, v.two, now); ok {
		d.steer = 0.4*d.steer + 0.6*x
		d.offSince = time.Time{}
		d.stillSince = now
		if btn == 1 {
			btn = a
		}
		return d.steer, btn
	}
	// Road following in single-player picture coordinates (y scaled for a half screen;
	// the shared two-player minimap covers x > 800 near the split line).
	sy := func(y int) int { return int(float64(y) * v.ys) }
	xmax := 900
	if v.two {
		xmax = 800
	}
	_, rows, mt, ok := readBand(v.path, v.top, sy(300), sy(560))
	if !ok {
		return 0.25 * math.Sin(float64(now.UnixMilli())/700.0), a
	}
	var nearSum, nearN, farSum, farN, nearTot float64
	var cur []float64
	for ri, row := range rows {
		y := int(float64(sy(300)+ri*4) / v.ys)
		for i, px := range row {
			x := i * 4
			if x < 380 || x > xmax {
				continue
			}
			lum := (float64(px[0]) + float64(px[1]) + float64(px[2])) / 3
			if y >= 400 && x >= 560 && x <= 720 {
				continue // the kart
			}
			cur = append(cur, lum)
			road := isRoad(px)
			if y >= 400 {
				nearTot++
				if road {
					nearSum += float64(x)
					nearN++
				}
			} else if road {
				farSum += float64(x)
				farN++
			}
		}
	}
	target := d.steer * 0.8
	if nearN > nearTot*0.25 {
		cx := nearSum / nearN
		if farN > 150 {
			cx = 0.6*cx + 0.4*farSum/farN
		}
		err := (cx - 640) / 180
		target = math.Max(-1, math.Min(1, err))
		if err < 0 {
			d.lastSeen = -1
		} else {
			d.lastSeen = 1
		}
		d.offSince = time.Time{}
	} else {
		if farN > 150 {
			if farSum/farN < 640 {
				d.lastSeen = -1
			} else {
				d.lastSeen = 1
			}
		}
		target = 0.9 * d.lastSeen
		if d.offSince.IsZero() {
			d.offSince = now
		} else if now.Sub(d.offSince) > 1800*time.Millisecond {
			d.reverseTil = now.Add(1300 * time.Millisecond)
			d.revSteer = -0.9 * d.lastSeen
			d.offSince = now.Add(2500 * time.Millisecond)
		}
	}
	d.steer = 0.5*d.steer + 0.5*target
	// Stuck: the view hardly changes for 2 s while A is held.
	if mt != d.lastMtime && d.prev != nil && len(d.prev) == len(cur) {
		var diff float64
		for i := range cur {
			diff += math.Abs(cur[i] - d.prev[i])
		}
		diff /= float64(len(cur))
		if diff > 4 {
			d.stillSince = now
		} else if d.stillSince.IsZero() {
			d.stillSince = now
		} else if now.Sub(d.stillSince) > 2000*time.Millisecond {
			d.reverseTil = now.Add(1300 * time.Millisecond)
			d.revSteer = -0.9 * d.lastSeen
			d.stillSince = now
		}
	}
	if mt != d.lastMtime {
		d.prev = cur
		d.lastMtime = mt
	}
	return d.steer, a
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}
