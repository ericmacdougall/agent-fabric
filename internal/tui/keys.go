package tui

import "unicode/utf8"

// KeyCode names a key the TUI understands.
type KeyCode int

const (
	KeyRune KeyCode = iota // a printable character in Rune
	KeyUp
	KeyDown
	KeyPgUp
	KeyPgDn
	KeyEnter
	KeyEsc
	KeyTab
	KeyBackTab
	KeyBackspace
	KeyCtrlC
	KeyCtrlF
	KeyUnknown
)

// Key is one decoded key press.
type Key struct {
	Code KeyCode
	Rune rune
}

// ParseKeys decodes one read from a raw-mode terminal. A read of exactly
// ESC is the Escape key; ESC followed by more bytes is a CSI or SS3
// sequence (arrows, page keys, back-tab), and unknown sequences are dropped
// whole so their bytes are never taken as typed characters.
func ParseKeys(b []byte) []Key {
	var ks []Key
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b && i+1 == len(b):
			ks = append(ks, Key{Code: KeyEsc})
			i++
		case c == 0x1b && (b[i+1] == '[' || b[i+1] == 'O'):
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) { // parameter bytes
				j++
			}
			if j >= len(b) {
				return append(ks, Key{Code: KeyUnknown})
			}
			seq := string(b[i+2 : j+1])
			ks = append(ks, Key{Code: csiKey(seq)})
			i = j + 1
		case c == 0x1b:
			ks = append(ks, Key{Code: KeyEsc})
			i++
		case c == '\r' || c == '\n':
			ks = append(ks, Key{Code: KeyEnter})
			i++
		case c == '\t':
			ks = append(ks, Key{Code: KeyTab})
			i++
		case c == 0x7f || c == 0x08:
			ks = append(ks, Key{Code: KeyBackspace})
			i++
		case c == 0x03:
			ks = append(ks, Key{Code: KeyCtrlC})
			i++
		case c == 0x06:
			ks = append(ks, Key{Code: KeyCtrlF})
			i++
		case c < 0x20:
			ks = append(ks, Key{Code: KeyUnknown})
			i++
		default:
			r, n := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && n <= 1 {
				ks = append(ks, Key{Code: KeyUnknown})
			} else {
				ks = append(ks, Key{Code: KeyRune, Rune: r})
			}
			i += n
		}
	}
	return ks
}

func csiKey(seq string) KeyCode {
	switch seq {
	case "A":
		return KeyUp
	case "B":
		return KeyDown
	case "5~":
		return KeyPgUp
	case "6~":
		return KeyPgDn
	case "Z":
		return KeyBackTab
	}
	return KeyUnknown
}

// KeyResult tells the run loop what a key changed.
type KeyResult struct {
	Refresh bool // the view changed in a way that needs a load (drill-down opened)
}

// pageRows is how far PgUp and PgDn move.
const pageRows = 10

// Handle applies one key. With keysOn false, single-character shortcuts
// (j, k, q, /) are ignored; the arrows, Enter, Esc, Tab, Ctrl-F and Ctrl-C
// still work, so the view stays fully usable (WCAG 2.1.4).
func (m *Model) Handle(k Key, keysOn bool) KeyResult {
	if k.Code == KeyCtrlC {
		m.Quit = true
		return KeyResult{}
	}
	if m.Filtering {
		switch k.Code {
		case KeyRune:
			if utf8.RuneCountInString(m.Filter) < 64 {
				m.Filter += string(k.Rune)
			}
		case KeyBackspace:
			if r := []rune(m.Filter); len(r) > 0 {
				m.Filter = string(r[:len(r)-1])
			}
		case KeyEnter:
			m.Filtering = false
		case KeyEsc:
			m.Filtering, m.Filter = false, ""
		}
		m.clamp()
		return KeyResult{}
	}
	code := k.Code
	if code == KeyRune {
		if !keysOn {
			return KeyResult{}
		}
		switch k.Rune {
		case 'j':
			code = KeyDown
		case 'k':
			code = KeyUp
		case 'q':
			m.Quit = true
			return KeyResult{}
		case '/':
			code = KeyCtrlF
		default:
			return KeyResult{}
		}
	}
	if m.DetailID != "" {
		switch code {
		case KeyEsc:
			m.DetailID, m.Detail, m.DetailSel = "", nil, 0
		case KeyUp:
			m.DetailSel--
		case KeyDown:
			m.DetailSel++
		case KeyPgUp:
			m.DetailSel -= pageRows
		case KeyPgDn:
			m.DetailSel += pageRows
		}
		m.clamp()
		return KeyResult{}
	}
	switch code {
	case KeyUp:
		m.Sel[m.Pane]--
	case KeyDown:
		m.Sel[m.Pane]++
	case KeyPgUp:
		m.Sel[m.Pane] -= pageRows
	case KeyPgDn:
		m.Sel[m.Pane] += pageRows
	case KeyTab:
		m.Pane = (m.Pane + 1) % paneCount
	case KeyBackTab:
		m.Pane = (m.Pane + paneCount - 1) % paneCount
	case KeyCtrlF:
		m.Filtering = true
	case KeyEsc:
		m.Filter = ""
	case KeyEnter:
		rs := m.rowsOf(m.Pane)
		if i := m.Sel[m.Pane]; i >= 0 && i < len(rs) && rs[i].mission != "" {
			m.DetailID, m.Detail, m.DetailSel = rs[i].mission, nil, 0
			return KeyResult{Refresh: true}
		}
	}
	m.clamp()
	return KeyResult{}
}

// clamp keeps selections inside their lists.
func (m *Model) clamp() {
	for p := Pane(0); p < paneCount; p++ {
		n := len(m.rowsOf(p))
		if m.Sel[p] >= n {
			m.Sel[p] = n - 1
		}
		if m.Sel[p] < 0 {
			m.Sel[p] = 0
		}
	}
	n := len(m.detailLines())
	if m.DetailSel >= n {
		m.DetailSel = n - 1
	}
	if m.DetailSel < 0 {
		m.DetailSel = 0
	}
}
