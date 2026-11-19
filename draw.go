package main

import (
	"fmt"
	"time"
)

var textPanels = map[string]func(*Stats, int) []line{
	"2": serverLines, "4": sourcesLines, "5": trackingLines, "6": netLines, "7": systemLines,
}

const defaultOffenderRows = 10

func columns(w int) [][]string {
	switch {
	case w >= 180:
		return layouts[3]
	case w >= 100:
		return layouts[2]
	}
	return layouts[1]
}

func (a *App) draw(w, h int) {
	a.scr.Reset(w, h)
	a.header(w)

	cols := columns(w)
	inner := w/len(cols) - 4
	content := map[string][]line{}
	for key, build := range textPanels {
		if a.visible[key] && a.panelAvailable(key) {
			content[key] = build(&a.stats, inner)
		}
	}
	if a.visible["3"] {
		content["3"] = offenderLines(&a.stats, inner, defaultOffenderRows, a.minPackets)
	}

	// Text panels get their natural height; the graph gets what's left (at least 4 rows).
	textH := 0
	for _, col := range cols {
		colH := 0
		for _, key := range col {
			if lines, ok := content[key]; ok {
				colH += len(lines) + 2
			}
		}
		textH = max(textH, colH)
	}
	top := 1
	if a.visible["1"] {
		gh := h - 1
		if textH > 0 {
			gh = max(4, h-1-textH)
		}
		a.requests(top, min(gh, h-1), w)
		top += gh
	}

	colW := w / len(cols)
	for i, col := range cols {
		x, cw := i*colW, colW
		if i == len(cols)-1 {
			cw = w - x
		}
		a.column(col, content, top, x, cw, h)
	}
	if a.help {
		a.drawHelp(w, h)
	}
}

func (a *App) column(col []string, content map[string][]line, y, x, cw, h int) {
	var keys []string
	for _, key := range col {
		if _, ok := content[key]; ok {
			keys = append(keys, key)
		}
	}
	for i, key := range keys {
		lines := content[key]
		ph := min(len(lines)+2, h-y)
		// Offenders grows to fill its column when it's the last panel in it.
		if key == "3" && i == len(keys)-1 && !a.stats.ClientsAt.IsZero() {
			lines = offenderLines(&a.stats, cw-4, h-y-4, a.minPackets)
			ph = min(len(lines)+2, h-y)
		}
		if ph < 3 {
			return
		}
		a.panel(y, x, ph, cw, key, title(key), lines)
		y += ph
	}
}

func (a *App) panel(y, x, h, w int, key, title string, lines []line) {
	a.scr.Box(y, x, h, w, key, title, "")
	for r, ln := range lines[:min(len(lines), h-2)] {
		lx := x + 2
		for _, s := range ln {
			a.scr.Put(y+1+r, lx, s.text, s.st, x+w-2-lx)
			lx += len([]rune(s.text))
		}
	}
}

func title(key string) string {
	for _, p := range panels {
		if p.key == key {
			return p.title
		}
	}
	return ""
}

func (a *App) header(w int) {
	left := fmt.Sprintf(" chronytop  %s", a.host)
	if a.network == "udp" {
		left += "  UDP"
	}
	if !isRemote(a.network, a.address) {
		left += fmt.Sprintf("  up %s  load %s", fmtDur(uptime()), loadAvg())
	}
	left += "  " + time.Now().UTC().Format("15:04:05 UTC")
	right := fmt.Sprintf("+/- %s  ? help  q quit ", a.interval)
	a.scr.Put(0, 0, left, bold, w)
	if len(left)+len(right)+2 <= w {
		a.scr.Put(0, w-len(right), right, dim, -1)
	}
}

func (a *App) requests(y, h, w int) {
	st := &a.stats
	if st.ServerErr != nil {
		a.panel(y, 0, h, w, "1", "requests", unavailable("Request counters", st.ServerErr))
		return
	}
	vis := st.Req
	if len(vis) > w-2 {
		vis = vis[len(vis)-(w-2):]
	}
	sum, peak := 0.0, 0.0
	for _, v := range vis {
		sum += v
		peak = max(peak, v)
	}
	avg := 0.0
	if len(vis) > 0 {
		avg = sum / float64(len(vis))
	}
	span := time.Duration(w-2) * a.interval
	extra := fmt.Sprintf(" %s req/s  avg %s  peak %s  drop %s/s  (%s) ", fmtSI(last(st.Req), ""),
		fmtSI(avg, ""), fmtSI(peak, ""), fmtSI(last(st.Drop), ""), fmtDur(span.Seconds()))
	a.scr.Box(y, 0, h, w, "1", "requests", extra)
	a.scr.Graph(y+1, 1, h-2, w-2, st.Req)
}

func (a *App) drawHelp(w, h int) {
	lines := []line{{{fmt.Sprintf("%-8s", "Key"), bold}, {"Action", bold}}}
	for _, p := range panels {
		if !a.panelAvailable(p.key) {
			continue
		}
		lines = append(lines, line{{fmt.Sprintf("%-8s", p.key), magenta}, {"Show/hide " + p.title, plain}})
	}
	for _, kv := range [][2]string{
		{"+ / -", "Slower / faster sampling"},
		{"? / h", "Show/hide this help"},
		{"Esc", "Close help"},
		{"q", "Quit"},
	} {
		lines = append(lines, line{{fmt.Sprintf("%-8s", kv[0]), magenta}, {kv[1], plain}})
	}
	ph, pw := min(h, len(lines)+2), min(w, 40)
	y, x := (h-ph)/2, (w-pw)/2
	for r := 0; r < ph; r++ {
		a.scr.Put(y+r, x, fmt.Sprintf("%*s", pw, ""), plain, -1)
	}
	a.panel(y, x, ph, pw, "0", "help", lines)
}
