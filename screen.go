package main

import (
	"fmt"
	"io"
	"strings"
)

// Style is SGR foreground and background colors (30-37 and 40-47, or 0 for default)
// plus bold and dim flags.
type Style struct {
	FG, BG    uint8
	Bold, Dim bool
}

var (
	plain   = Style{}
	dim     = Style{Dim: true}
	bold    = Style{Bold: true}
	red     = Style{FG: 31}
	green   = Style{FG: 32}
	yellow  = Style{FG: 33}
	blue    = Style{FG: 34}
	magenta = Style{FG: 35}
	cyan    = Style{FG: 36}
)

func (s Style) bold() Style { s.Bold = true; return s }

func (s Style) sgr() string {
	var b strings.Builder
	b.WriteString("\x1b[0")
	if s.Bold {
		b.WriteString(";1")
	}
	if s.Dim {
		b.WriteString(";2")
	}
	if s.FG != 0 {
		fmt.Fprintf(&b, ";%d", s.FG)
	}
	if s.BG != 0 {
		fmt.Fprintf(&b, ";%d", s.BG)
	}
	b.WriteByte('m')
	return b.String()
}

type cell struct {
	r  rune
	st Style
}

// Screen is a frame buffer drawn to the terminal in one write.
type Screen struct {
	W, H  int
	cells []cell
}

func (s *Screen) Reset(w, h int) {
	s.W, s.H = w, h
	s.cells = make([]cell, w*h)
	for i := range s.cells {
		s.cells[i] = cell{r: ' '}
	}
}

// Put writes text at y, x, clipped to maxw columns (no limit if maxw < 0) and the screen.
func (s *Screen) Put(y, x int, text string, st Style, maxw int) {
	if y < 0 || y >= s.H {
		return
	}
	for _, r := range text {
		if maxw == 0 || x >= s.W {
			return
		}
		if x >= 0 {
			s.cells[y*s.W+x] = cell{r, st}
		}
		x++
		maxw--
	}
}

func (s *Screen) Box(y, x, h, w int, key, title, extra string) {
	s.Put(y, x, "╭"+strings.Repeat("─", w-2)+"╮", blue, -1)
	for r := 1; r < h-1; r++ {
		s.Put(y+r, x, "│", blue, -1)
		s.Put(y+r, x+w-1, "│", blue, -1)
	}
	s.Put(y+h-1, x, "╰"+strings.Repeat("─", w-2)+"╯", blue, -1)
	s.Put(y, x+2, superscript(key), magenta.bold(), -1)
	s.Put(y, x+3, title, bold, -1)
	if n := len([]rune(extra)); extra != "" && n+len(title)+8 < w {
		s.Put(y, x+w-n-3, extra, dim, -1)
	}
}

const blocks = " ▁▂▃▄▅▆▇█"

// Graph plots the newest values right-aligned, scaled to the visible peak. If part is
// non-nil, it holds the share of each value (aligned with data's newest entries) to stack
// at the bottom of each bar in partSt, with the rest in st.
func (s *Screen) Graph(y, x, h, w int, data, part []float64, st, partSt Style) {
	if len(data) > w {
		data = data[len(data)-w:]
	}
	peak := 0.0
	for _, v := range data {
		peak = max(peak, v)
	}
	if peak == 0 {
		peak = 1
	}
	bars := []rune(blocks)
	off := w - len(data)
	partOff := len(part) - len(data) // index into part for data[0]
	for i, v := range data {
		eighths := int(v/peak*float64(h*8) + 0.5)
		partEighths := 0
		if j := partOff + i; j >= 0 && j < len(part) {
			partEighths = min(eighths, int(part[j]/peak*float64(h*8)+0.5))
		}
		for r := 0; r < h; r++ {
			fill := min(8, max(0, eighths-r*8))
			if fill == 0 {
				break
			}
			pf := min(8, max(0, partEighths-r*8))
			cs := st
			switch {
			case pf >= fill:
				cs = partSt
			case pf > 0 && fill == 8:
				// The split falls inside a full cell: the lower pf eighths in partSt, the rest as background.
				fill, cs = pf, Style{FG: partSt.FG, BG: st.FG + 10}
			case pf*2 >= fill:
				// The bar ends in the same cell as the split; use the larger share's color.
				cs = partSt
			}
			s.Put(y+h-1-r, x+off+i, string(bars[fill]), cs, -1)
		}
	}
	s.Put(y, x, fmtSI(peak, ""), dim, -1)
	s.Put(y+h-1, x, "0", dim, -1)
}

func (s *Screen) Flush(out io.Writer) error {
	var b strings.Builder
	cur := Style{FG: 255}
	for y := 0; y < s.H; y++ {
		fmt.Fprintf(&b, "\x1b[%d;1H", y+1)
		for _, c := range s.cells[y*s.W : (y+1)*s.W] {
			if c.st != cur {
				b.WriteString(c.st.sgr())
				cur = c.st
			}
			b.WriteRune(c.r)
		}
	}
	b.WriteString("\x1b[0m")
	_, err := io.WriteString(out, b.String())
	return err
}

func superscript(key string) string {
	const digits = "⁰¹²³⁴⁵⁶⁷⁸⁹"
	if len(key) == 1 && key[0] >= '0' && key[0] <= '9' {
		return string([]rune(digits)[key[0]-'0'])
	}
	return key
}
