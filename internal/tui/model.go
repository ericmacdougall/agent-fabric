package tui

import (
	"context"
	"strings"
	"time"
)

// Pane is one of the four list views.
type Pane int

const (
	PaneMissions Pane = iota
	PaneAttempts
	PaneEffects
	PaneAsks
	paneCount
)

var paneNames = [paneCount]string{"Missions", "Attempts", "Effects", "Asks"}

// Model is everything a frame is rendered from. It holds no connection.
type Model struct {
	Data   *Data     // last successful refresh (nil before the first)
	LastOK time.Time // local clock at that refresh
	Fail   *Failure  // set while the latest refresh failed

	Pane      Pane
	Sel       [paneCount]int
	Filter    string
	Filtering bool // typing a filter

	DetailID  string  // drill-down mission ("" in the lists)
	Detail    *Detail // loaded drill-down (nil until loaded)
	DetailSel int

	JobCap int64 // per-job spending ceiling shown against a mission's spend (USD micros)

	Quit bool
}

// Apply records a refresh result. On failure the last data is kept, so the
// operator still sees it, marked with its age.
func (m *Model) Apply(d *Data, det *Detail, err error, now time.Time) {
	if err != nil {
		f := classify(err)
		m.Fail = &f
		return
	}
	m.Data, m.LastOK, m.Fail = d, now, nil
	if m.DetailID != "" {
		m.Detail = det
	}
}

// Session couples a Model with its Source; Refresh is the synchronous form
// the run loop performs in the background.
type Session struct {
	Src   *Source
	M     Model
	Clock func() time.Time
}

// Refresh fetches and applies one refresh.
func (s *Session) Refresh(ctx context.Context) error {
	d, det, err := s.Src.Fetch(ctx, s.M.DetailID)
	now := time.Now()
	if s.Clock != nil {
		now = s.Clock()
	}
	s.M.Apply(d, det, err, now)
	return err
}

// rowsOf returns the visible rows of a pane after the filter: the mission
// id each row drills into and its searchable text.
func (m *Model) rowsOf(p Pane) []row {
	if m.Data == nil {
		return nil
	}
	var rs []row
	add := func(mission string, cells ...string) {
		r := row{mission: mission, cells: cells}
		if m.Filter == "" || strings.Contains(strings.ToLower(Clean(strings.Join(cells, " "), 0)), strings.ToLower(Clean(m.Filter, 0))) {
			rs = append(rs, r)
		}
	}
	now := m.Data.Now
	switch p {
	case PaneMissions:
		for _, x := range m.Data.Missions {
			add(x.ID, x.ID, x.Execution, x.Terminal, age(now, x.Created), spend(x.Spent, m.jobCap()), x.Ticket)
		}
	case PaneAttempts:
		for _, x := range m.Data.Attempts {
			add(x.Mission, x.ID, x.Mission, x.State, itoa(x.Fence), lease(now, x.Lease))
		}
	case PaneEffects:
		for _, x := range m.Data.Effects {
			add(x.Mission, x.ID, x.Mission, x.Kind, x.State, age(now, x.Updated), itoa(x.Dispatches))
		}
	case PaneAsks:
		for _, x := range m.Data.Asks {
			add(x.Mission, x.ID, x.Mission, x.Kind, age(now, x.Created), x.Question)
		}
	}
	return rs
}

func (m *Model) jobCap() int64 { return m.JobCap }

type row struct {
	mission string
	cells   []string
}

// detailLines are the drill-down's selectable lines.
func (m *Model) detailLines() []string {
	d := m.Detail
	if d == nil || m.Data == nil {
		return nil
	}
	if !d.Found {
		return []string{"mission " + m.DetailID + " not found"}
	}
	now := m.Data.Now
	x := d.Mission
	ls := []string{
		"Mission " + x.ID + "  " + x.Execution + "/" + x.Terminal + "  age " + age(now, x.Created) + "  spent " + spend(x.Spent, m.jobCap()),
		"Ticket: " + x.Ticket,
		"Attempts (" + itoa(int64(len(d.Attempts))) + ")",
	}
	for _, a := range d.Attempts {
		ls = append(ls, "  "+a.ID+"  fence "+itoa(a.Fence)+"  "+a.State+"  "+lease(now, a.Lease))
	}
	ls = append(ls, "Effects ("+itoa(int64(len(d.Effects)))+")")
	for _, e := range d.Effects {
		ls = append(ls, "  "+e.ID+"  "+e.Kind+"  "+e.State+"  disp "+itoa(e.Dispatches)+"  age "+age(now, e.Updated))
	}
	ls = append(ls, "Asks ("+itoa(int64(len(d.Asks)))+")")
	for _, a := range d.Asks {
		ls = append(ls, "  "+a.ID+"  "+a.Kind+"  "+a.State+"  age "+age(now, a.Created)+"  "+a.Question)
	}
	var decisions []string
	for _, a := range d.Asks {
		if a.Kind == "decision" {
			decisions = append(decisions, "  ask "+a.ID+"  "+a.State)
		}
	}
	for _, r := range d.Resolutions {
		decisions = append(decisions, "  "+r.Effect+"  "+r.From+" -> "+r.To+"  by "+r.Actor+"  "+age(now, r.At)+" ago")
	}
	ls = append(ls, "Decisions ("+itoa(int64(len(decisions)))+")")
	return append(ls, decisions...)
}
