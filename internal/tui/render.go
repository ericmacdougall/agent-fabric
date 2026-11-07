package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Opts are the presentation switches.
type Opts struct {
	Color  bool // SGR bold, reverse and red/yellow; off with --no-color or NO_COLOR
	KeysOn bool // single-character shortcuts enabled (--keys=on)
}

// The only escape sequences a frame may contain, and only when Color is set.
const (
	sgrBold    = "\x1b[1m"
	sgrReverse = "\x1b[7m"
	sgrRed     = "\x1b[31m"
	sgrYellow  = "\x1b[33m"
	sgrReset   = "\x1b[0m"
)

// MinWidth and MinHeight are the smallest terminal a frame is drawn for.
const (
	MinWidth  = 20
	MinHeight = 6
)

// col is one table column: its header, width, and drop priority (lower
// numbers survive a narrow terminal longer). The flex column takes what is
// left.
type col struct {
	name     string
	width    int
	priority int
	flex     bool
}

var paneCols = [paneCount][]col{
	PaneMissions: {{"ID", 14, 0, false}, {"EXEC", 9, 1, false}, {"TERM", 9, 3, false}, {"AGE", 5, 2, false}, {"SPENT/CAP", 13, 4, false}, {"TICKET", 0, 0, true}},
	PaneAttempts: {{"ID", 14, 0, false}, {"MISSION", 14, 1, false}, {"STATE", 9, 0, false}, {"FENCE", 5, 3, false}, {"LEASE", 0, 0, true}},
	PaneEffects:  {{"ID", 14, 0, false}, {"MISSION", 14, 2, false}, {"KIND", 14, 3, false}, {"STATE", 11, 0, false}, {"AGE", 5, 1, false}, {"DISP", 0, 4, true}},
	PaneAsks:     {{"ID", 14, 0, false}, {"MISSION", 14, 2, false}, {"KIND", 10, 3, false}, {"AGE", 5, 1, false}, {"QUESTION", 0, 0, true}},
}

// Render draws one frame of exactly h lines, each at most w columns wide.
// It is a pure function of m, now (the local clock, for the data's age) and
// o. Every value from the database passes through Clean.
func Render(m *Model, w, h int, now time.Time, o Opts) []string {
	lines := make([]string, h)
	if w < MinWidth || h < MinHeight {
		if h > 0 {
			lines[0] = Fit("terminal too small", w)
		}
		return lines
	}
	style := func(s string, codes ...string) string {
		if !o.Color || len(codes) == 0 {
			return s
		}
		return strings.Join(codes, "") + s + sgrReset
	}

	// Line 0: title and connection banner.
	title := "fab tui"
	if m.Data != nil {
		title += "  " + m.Data.Now.UTC().Format("15:04:05Z")
	}
	lines[0] = style(Pad(title, w), sgrBold)
	if m.Fail != nil {
		var msg string
		dataAge := "no data yet"
		if m.Data != nil {
			dataAge = "data " + dur(now.Sub(m.LastOK)) + " old"
		}
		if m.Fail.Disconnected {
			msg = "! disconnected: retrying (" + m.Fail.String() + "; " + dataAge + ")"
			lines[0] = style(Pad(msg, w), sgrBold, sgrRed)
		} else {
			msg = "! stale: refresh failed (" + m.Fail.String() + "; " + dataAge + ")"
			lines[0] = style(Pad(msg, w), sgrBold, sgrYellow)
		}
	}

	// Lines 1 and 2: the /metrics gauges.
	if m.Data != nil {
		hd := m.Data.Header
		lines[1] = Clean(fmt.Sprintf("%s s%d | missions %d/%d active | attempts %d live %d stale | asks %d",
			hd.Version, hd.Schema, hd.MissionsActive, hd.Missions, hd.AttemptsLive, hd.AttemptsStale, hd.AsksOpen), w)
		lines[2] = Clean(fmt.Sprintf("effects %d unk %d quar %d rec | dispatch %d/%d %d late | month %s",
			hd.Unknown, hd.Quarantined, hd.Reconciling, hd.Inflight, hd.Limit, hd.Overdue, spend(hd.SpentMonth, hd.MonthCap)), w)
	} else {
		lines[1] = Fit("loading...", w)
	}

	body := lines[3 : h-1]
	if m.DetailID != "" {
		renderDetail(m, body, w, style)
	} else {
		renderPane(m, body, w, style)
	}

	// Last line: keys or the filter being typed.
	var foot string
	switch {
	case m.Filtering:
		foot = "filter: " + Clean(m.Filter, 0) + "_  (Enter apply, Esc clear)"
	case m.DetailID != "":
		foot = keyHelp(o.KeysOn, "Esc back")
	default:
		foot = keyHelp(o.KeysOn, "Enter open  Tab pane")
		if m.Filter != "" {
			foot = "[filter: " + Clean(m.Filter, 0) + "]  " + foot
		}
	}
	lines[h-1] = Fit(foot, w)
	return lines
}

func keyHelp(on bool, extra string) string {
	if on {
		return "j/k move  " + extra + "  / filter  q quit"
	}
	return "Up/Down move  " + extra + "  Ctrl-F filter  Ctrl-C quit"
}

func renderPane(m *Model, body []string, w int, style func(string, ...string) string) {
	if len(body) < 2 {
		return
	}
	var tabs []string
	for p := Pane(0); p < paneCount; p++ {
		label := paneNames[p] + " " + strconv.Itoa(len(m.rowsOf(p)))
		if p == m.Pane {
			label = "[" + label + "]"
		} else {
			label = " " + label + " "
		}
		tabs = append(tabs, label)
	}
	body[0] = style(Pad(strings.Join(tabs, " "), w), sgrBold)

	cols := fitCols(paneCols[m.Pane], w-2)
	var hdr []string
	for _, c := range cols {
		hdr = append(hdr, Pad(c.name, c.width))
	}
	body[1] = Fit("  "+strings.Join(hdr, " "), w)

	rows := m.rowsOf(m.Pane)
	space := len(body) - 2
	if len(rows) == 0 {
		if space > 0 {
			body[2] = Fit("  (none)", w)
		}
		return
	}
	sel := m.Sel[m.Pane]
	top := 0
	if sel >= space {
		top = sel - space + 1
	}
	for i := 0; i < space && top+i < len(rows); i++ {
		r := rows[top+i]
		var cells []string
		for _, c := range cols {
			cells = append(cells, Pad(Clean(r.cells[c.index], 0), c.width))
		}
		marker := "  "
		if top+i == sel {
			marker = "> "
		}
		line := Pad(marker+strings.Join(cells, " "), w)
		if top+i == sel {
			line = style(line, sgrReverse)
		}
		body[2+i] = line
	}
}

func renderDetail(m *Model, body []string, w int, style func(string, ...string) string) {
	ls := m.detailLines()
	if ls == nil {
		body[0] = Fit("loading mission "+Clean(m.DetailID, 0)+"...", w)
		return
	}
	top := 0
	if m.DetailSel >= len(body) {
		top = m.DetailSel - len(body) + 1
	}
	for i := 0; i < len(body) && top+i < len(ls); i++ {
		marker := "  "
		if top+i == m.DetailSel {
			marker = "> "
		}
		line := Pad(marker+Clean(ls[top+i], 0), w)
		if top+i == m.DetailSel {
			line = style(line, sgrReverse)
		}
		body[i] = line
	}
}

// placedCol is a column chosen for a width, with its final width and the
// index of its cell in a row.
type placedCol struct {
	name  string
	width int
	index int
}

// fitCols drops the lowest-priority columns until the rest fit in w (one
// space between columns) and gives the flex column what is left, at least 4.
func fitCols(cs []col, w int) []placedCol {
	keep := make([]bool, len(cs))
	for i := range cs {
		keep[i] = true
	}
	need := func() int {
		n, k := 0, 0
		for i, c := range cs {
			if keep[i] {
				if c.flex {
					n += 4
				} else {
					n += c.width
				}
				k++
			}
		}
		return n + k - 1
	}
	for need() > w {
		worst := -1
		for i, c := range cs {
			if keep[i] && !c.flex && (worst < 0 || c.priority > cs[worst].priority || c.priority == cs[worst].priority && i > worst) {
				worst = i
			}
		}
		if worst < 0 || cs[worst].priority == 0 && countKept(keep) <= 2 {
			break
		}
		keep[worst] = false
	}
	var out []placedCol
	used := 0
	for i, c := range cs {
		if keep[i] {
			out = append(out, placedCol{name: c.name, width: c.width, index: i})
			if !c.flex {
				used += c.width + 1
			}
		}
	}
	for i := range out {
		if cs[out[i].index].flex {
			out[i].width = w - used
			if out[i].width < 1 {
				out[i].width = 1
			}
		}
	}
	return out
}

func countKept(k []bool) int {
	n := 0
	for _, b := range k {
		if b {
			n++
		}
	}
	return n
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// dur is a compact non-negative duration: 42s, 5m, 3h, 2d.
func dur(d time.Duration) string {
	s := int64(d / time.Second)
	switch {
	case s < 0:
		return "0s"
	case s < 60:
		return itoa(s) + "s"
	case s < 3600:
		return itoa(s/60) + "m"
	case s < 86400:
		return itoa(s/3600) + "h"
	}
	return itoa(s/86400) + "d"
}

func age(now, t time.Time) string { return dur(now.Sub(t)) }

func lease(now time.Time, until *time.Time) string {
	if until == nil {
		return "none"
	}
	if until.After(now) {
		return "live " + dur(until.Sub(now))
	}
	return "stale " + dur(now.Sub(*until))
}

// spend is micros over a cap in dollars; the cap is omitted when unknown.
func spend(micros, capMicros int64) string {
	s := fmt.Sprintf("$%.2f", float64(micros)/1e6)
	if capMicros > 0 {
		s += fmt.Sprintf("/$%.2f", float64(capMicros)/1e6)
	}
	return s
}
