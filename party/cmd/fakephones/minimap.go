package main

// Minimap pursuit for Luigi Circuit, so that test bots can finish a race.
//
// The minimap sits at a fixed place (picture x 860..1260, y 220..660; the frame file
// has the picture at rows 186..906) with a fixed orientation. The kart's own icon is
// drawn larger than the others; it is found by normalized correlation with a crop of
// that character's icon (<character>.png, 35x35, cut from race captures by `./mkparty
// calibrate` on the user's own machine, see iconDir: these game-derived pictures are never
// in the git repository; a bot without its icon drives without steering). The
// heading is the direction of the translucent yellow view cone around the icon. The
// course's centre line below was traced from captures (minimap coordinates, race
// direction: up the left straight from the start line, clockwise).

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const mmX0, mmY0, mmX1, mmY1 = 860, 220, 1260, 660
const mmW, mmH = mmX1 - mmX0, mmY1 - mmY0
const iconR = 17

// iconDir holds the minimap icons: $FAKEPHONES_ICONS, else the icons folder of the assets
// that `./mkparty calibrate` generates from the user's own game ($MKPARTY_ASSETS/icons, or
// assets/icons at the project root), else party/cmd/fakephones/icons (older installs),
// found from the binary (run/bin/fakephones) or the current directory.
func iconDir() string {
	if d := os.Getenv("FAKEPHONES_ICONS"); d != "" {
		return d
	}
	var cands []string
	if d := os.Getenv("MKPARTY_ASSETS"); d != "" {
		cands = append(cands, filepath.Join(d, "icons"))
	}
	if exe, err := os.Executable(); err == nil {
		root := filepath.Join(filepath.Dir(exe), "..", "..")
		cands = append(cands, filepath.Join(root, "assets", "icons"), filepath.Join(root, "party", "cmd", "fakephones", "icons"))
	}
	cands = append(cands, filepath.Join("..", "assets", "icons"), filepath.Join("assets", "icons"),
		filepath.Join("cmd", "fakephones", "icons"), filepath.Join("party", "cmd", "fakephones", "icons"))
	for _, d := range cands {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return cands[0]
}

var luigiLine = [][2]float64{
	{83, 330}, {83, 230}, {83, 140}, {92, 115}, {115, 100}, {150, 90}, {190, 75}, {220, 62},
	{245, 55}, {265, 62}, {272, 80}, {265, 105}, {240, 135}, {205, 175}, {175, 215}, {165, 240},
	{165, 290}, {165, 330}, {155, 355}, {125, 367}, {95, 355},
}

// icon template: masked, mean-removed samples at offsets (dx,dy) within radius iconR.
type iconTpl struct {
	dx, dy []int
	v      []float64 // 3 values per offset, mean removed
	norm   float64
}

var tplCache = map[string]*iconTpl{}

func loadTpl(char string) *iconTpl {
	if t, ok := tplCache[char]; ok {
		return t
	}
	tplCache[char] = nil
	p := filepath.Join(iconDir(), char+".png")
	b, err := os.ReadFile(p)
	if err != nil {
		log.Printf("minimap: no icon for %s (%v); this bot will not steer. The icons are generated from your own game by ./mkparty calibrate (README.md, 'Game assets').", char, err)
		return nil
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	t := &iconTpl{}
	bd := img.Bounds()
	var sum float64
	for dy := -iconR; dy <= iconR; dy += 2 {
		for dx := -iconR; dx <= iconR; dx += 2 {
			if dx*dx+dy*dy > iconR*iconR {
				continue
			}
			r, g, bl, _ := img.At(bd.Min.X+iconR+dx, bd.Min.Y+iconR+dy).RGBA()
			t.dx = append(t.dx, dx)
			t.dy = append(t.dy, dy)
			for _, c := range []uint32{r, g, bl} {
				f := float64(c >> 8)
				t.v = append(t.v, f)
				sum += f
			}
		}
	}
	m := sum / float64(len(t.v))
	for i := range t.v {
		t.v[i] -= m
		t.norm += t.v[i] * t.v[i]
	}
	t.norm = math.Sqrt(t.norm)
	tplCache[char] = t
	return t
}

// minimap holds the RGB pixels of the minimap region of one frame (w*h*3).
type minimap struct {
	px   []float64
	w, h int
}

func (m *minimap) at(x, y int) (float64, float64, float64) {
	i := (y*m.w + x) * 3
	return m.px[i], m.px[i+1], m.px[i+2]
}

// Two-player split screen (4:3, picture = frame rows 56..1016): ONE minimap for both
// players straddles the split line, drawn 1.2x larger and rotated a quarter turn
// clockwise relative to the single-player one (measured 2026-10-14 on Luigi Circuit:
// the start at single-player minimap (84, 355) is at picture (855, 445)). readMinimap2p
// samples it upright at 1/1.2 scale (so the icons are the single-player size) into a
// buffer whose pixel (x, y) is picture (mm2X0 + 1.2x, mm2Y0 + 1.2y); to2p / from2p
// convert to the single-player minimap coordinates of luigiLine.
const (
	mm2X0, mm2Y0 = 800, 400
	mm2S         = 1.2
	mm2W, mm2H   = 400, 260
	mm2OX, mm2OY = 1281.0, 344.0 // picture X = OX - S*y1, picture Y = OY + S*x1
)

func from2p(x, y float64) (float64, float64) {
	px, py := mm2X0+mm2S*x, mm2Y0+mm2S*y
	return (py - mm2OY) / mm2S, (mm2OX - px) / mm2S
}

// heading1p turns a direction in the two-player buffer into single-player minimap terms.
func heading1p(a float64) float64 {
	return math.Atan2(-math.Cos(a), math.Sin(a))
}

func readMinimap(path string) (*minimap, time.Time, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 54 {
		return nil, time.Time{}, false
	}
	off := int(binary.LittleEndian.Uint32(b[10:]))
	width := int(int32(binary.LittleEndian.Uint32(b[18:])))
	height := int(int32(binary.LittleEndian.Uint32(b[22:])))
	bottomUp := height > 0
	if height < 0 {
		height = -height
	}
	if width < mmX1 || height < 186+mmY1 {
		return nil, time.Time{}, false
	}
	stride := (width*3 + 3) &^ 3
	m := &minimap{px: make([]float64, mmW*mmH*3), w: mmW, h: mmH}
	for y := 0; y < mmH; y++ {
		fy := 186 + mmY0 + y
		ry := fy
		if bottomUp {
			ry = height - 1 - fy
		}
		base := off + ry*stride + mmX0*3
		if base+mmW*3 > len(b) {
			return nil, time.Time{}, false
		}
		for x := 0; x < mmW; x++ {
			p := base + x*3
			i := (y*mmW + x) * 3
			m.px[i], m.px[i+1], m.px[i+2] = float64(b[p+2]), float64(b[p+1]), float64(b[p])
		}
	}
	return m, st.ModTime(), true
}

func readMinimap2p(path string) (*minimap, time.Time, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 54 {
		return nil, time.Time{}, false
	}
	off := int(binary.LittleEndian.Uint32(b[10:]))
	width := int(int32(binary.LittleEndian.Uint32(b[18:])))
	height := int(int32(binary.LittleEndian.Uint32(b[22:])))
	bottomUp := height > 0
	if height < 0 {
		height = -height
	}
	if width < 1280 || height < 1016 {
		return nil, time.Time{}, false
	}
	stride := (width*3 + 3) &^ 3
	m := &minimap{px: make([]float64, mm2W*mm2H*3), w: mm2W, h: mm2H}
	for y := 0; y < mm2H; y++ {
		fy := 56 + mm2Y0 + int(mm2S*float64(y))
		ry := fy
		if bottomUp {
			ry = height - 1 - fy
		}
		for x := 0; x < mm2W; x++ {
			fx := mm2X0 + int(mm2S*float64(x))
			if fx >= width {
				fx = width - 1
			}
			p := off + ry*stride + fx*3
			if p+3 > len(b) {
				return nil, time.Time{}, false
			}
			i := (y*mm2W + x) * 3
			m.px[i], m.px[i+1], m.px[i+2] = float64(b[p+2]), float64(b[p+1]), float64(b[p])
		}
	}
	return m, st.ModTime(), true
}

func (m *minimap) ncc(t *iconTpl, x, y int) float64 {
	n := len(t.dx)
	var sum float64
	vals := make([]float64, 0, n*3)
	for k := 0; k < n; k++ {
		r, g, b := m.at(x+t.dx[k], y+t.dy[k])
		vals = append(vals, r, g, b)
		sum += r + g + b
	}
	mean := sum / float64(len(vals))
	var dot, nn float64
	for i, v := range vals {
		v -= mean
		dot += v * t.v[i]
		nn += v * v
	}
	return dot / (math.Sqrt(nn)*t.norm + 1e-6)
}

// locate finds the own icon; it searches around the last position first.
func (m *minimap) locate(t *iconTpl, lx, ly int, have bool) (int, int, float64) {
	search := func(x0, y0, x1, y1, step int) (int, int, float64) {
		bs, bx, by := -2.0, 0, 0
		x0, y0 = max(x0, iconR), max(y0, iconR)
		x1, y1 = min(x1, m.w-iconR-1), min(y1, m.h-iconR-1)
		for y := y0; y <= y1; y += step {
			for x := x0; x <= x1; x += step {
				if s := m.ncc(t, x, y); s > bs {
					bs, bx, by = s, x, y
				}
			}
		}
		return bx, by, bs
	}
	if have {
		x, y, s := search(lx-40, ly-40, lx+40, ly+40, 2)
		if s > 0.8 {
			x, y, s = search(x-1, y-1, x+1, y+1, 1)
			return x, y, s
		}
	}
	x, y, s := search(0, 0, m.w, m.h, 3)
	x, y, s = search(x-2, y-2, x+2, y+2, 1)
	return x, y, s
}

// heading returns the direction of the view cone (radians, image coordinates: y down)
// and its contrast over the surrounding ring.
func (m *minimap) heading(x, y int) (float64, float64) {
	const bins = 72
	prof := make([]float64, bins)
	for b := 0; b < bins; b++ {
		a := float64(b) * 2 * math.Pi / bins
		var s float64
		var n int
		for r := 26.0; r < 52; r += 2 {
			px := int(math.Round(float64(x) + r*math.Cos(a)))
			py := int(math.Round(float64(y) + r*math.Sin(a)))
			if px < 0 || py < 0 || px >= m.w || py >= m.h {
				continue
			}
			rr, g, bl := m.at(px, py)
			s += (rr+g)/2 - bl
			n++
		}
		if n > 0 {
			prof[b] = s / float64(n)
		}
	}
	sorted := append([]float64(nil), prof...)
	sort.Float64s(sorted)
	med := sorted[bins/2]
	best, bb := -1e9, 0
	for b := 0; b < bins; b++ {
		var w float64
		for k := -4; k <= 4; k++ {
			w += prof[(b+k+bins)%bins] - med
		}
		if w > best {
			best, bb = w, b
		}
	}
	return float64(bb) * 2 * math.Pi / bins, best / 9
}

type mmState struct {
	char     string
	lx, ly   int
	have     bool
	lastOK   time.Time
	lastMt   time.Time
	hist     []mmPoint
	moved    bool // the kart has moved since the race started (not the countdown)
	revUntil time.Time
	revSteer float64
	fwdSince time.Time
	lastOut  float64
	lastBtn  int
}

type mmPoint struct {
	t    time.Time
	x, y float64
}

// target returns the point lookahead px further along the line from the nearest point.
func lineTarget(x, y, lookahead float64) (float64, float64, float64) {
	best, bi, bt := math.MaxFloat64, 0, 0.0
	nl := len(luigiLine)
	for i := 0; i < nl; i++ {
		a, c := luigiLine[i], luigiLine[(i+1)%nl]
		dx, dy := c[0]-a[0], c[1]-a[1]
		l2 := dx*dx + dy*dy
		t := ((x-a[0])*dx + (y-a[1])*dy) / l2
		t = math.Max(0, math.Min(1, t))
		px, py := a[0]+t*dx, a[1]+t*dy
		if d := math.Hypot(x-px, y-py); d < best {
			best, bi, bt = d, i, t
		}
	}
	need := lookahead
	i, t := bi, bt
	for {
		a, c := luigiLine[i], luigiLine[(i+1)%nl]
		seg := math.Hypot(c[0]-a[0], c[1]-a[1])
		rest := (1 - t) * seg
		if rest >= need {
			f := t + need/seg
			return a[0] + f*(c[0]-a[0]), a[1] + f*(c[1]-a[1]), best
		}
		need -= rest
		i = (i + 1) % nl
		t = 0
	}
}

// pursue returns stick x and buttons (1 = A, 2 = B) for the minimap pursuit, or
// ok=false when it does not apply (unknown character, no minimap, icon not found).
func (m *mmState) pursue(path string, two bool, now time.Time) (float64, int, bool) {
	t := loadTpl(m.char)
	if t == nil {
		return 0, 0, false
	}
	if now.Sub(m.lastOK) > 3*time.Second {
		// new race (or back from menus): forget the past
		m.have, m.hist, m.moved = false, nil, false
		m.revUntil, m.fwdSince = time.Time{}, now
	}
	st, err := os.Stat(path)
	if err == nil && st.ModTime().Equal(m.lastMt) && now.Sub(m.lastOK) < 400*time.Millisecond {
		return m.lastOut, m.lastBtn, true // same frame as last time
	}
	read := readMinimap
	if two {
		read = readMinimap2p
	}
	mm, mt, ok := read(path)
	if !ok {
		return 0, 0, false
	}
	x, y, s := mm.locate(t, m.lx, m.ly, m.have)
	if s < 0.75 {
		m.have = false
		return 0, 0, false
	}
	m.lx, m.ly, m.have, m.lastOK, m.lastMt = x, y, true, now, mt
	fx, fy := float64(x), float64(y)
	if two {
		fx, fy = from2p(fx, fy)
	}
	m.hist = append(m.hist, mmPoint{now, fx, fy})
	for len(m.hist) > 1 && now.Sub(m.hist[0].t) > 1800*time.Millisecond {
		m.hist = m.hist[1:]
	}
	if h := m.hist[0]; math.Hypot(fx-h.x, fy-h.y) > 8 {
		m.moved = true
	}
	head, contrast := mm.heading(x, y)
	if two {
		head = heading1p(head)
	}
	tx, ty, _ := lineTarget(fx, fy, 32)
	want := math.Atan2(ty-fy, tx-fx)
	d := want - head
	for d > math.Pi {
		d -= 2 * math.Pi
	}
	for d < -math.Pi {
		d += 2 * math.Pi
	}
	if contrast < 15 {
		d = 0 // no clear cone: go straight on
	}
	// y grows downwards: a positive angle difference is a clockwise (right) turn.
	steer := math.Max(-1, math.Min(1, d/0.6))
	btn := 1
	if now.Before(m.revUntil) {
		steer, btn = m.revSteer, 2
	} else {
		// stuck: hardly moved for 1.8 s while driving forwards (after the start)
		h := m.hist[0]
		if m.moved && now.Sub(m.fwdSince) > 2*time.Second && now.Sub(h.t) > 1500*time.Millisecond &&
			math.Hypot(fx-h.x, fy-h.y) < 5 {
			sgn := 1.0
			if d < 0 {
				sgn = -1
			}
			m.revSteer = -sgn // backing up with the stick the other way turns the nose towards d
			m.revUntil = now.Add(1100 * time.Millisecond)
			m.fwdSince = m.revUntil
			m.hist = nil
			steer, btn = m.revSteer, 2
		}
	}
	m.lastOut, m.lastBtn = steer, btn
	return steer, btn, true
}
