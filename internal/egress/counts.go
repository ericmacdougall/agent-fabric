package egress

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
)

// HostCounts is the per-destination traffic of one run (goodqa#15966
// AC-20): tunnels established, CONNECTs refused before a tunnel, and bytes
// relayed in each direction. RefusedHost "-" collects CONNECTs to a
// destination outside the policy (the host name is attacker-chosen and is
// never recorded).
type HostCounts struct {
	Host        string `json:"host"`
	Connections int64  `json:"connections"`
	Refused     int64  `json:"refused"`
	BytesUp     int64  `json:"bytes_up"`   // worker to destination
	BytesDown   int64  `json:"bytes_down"` // destination to worker
}

// RefusedHost labels refused CONNECTs to a destination outside the policy.
const RefusedHost = "-"

type hostCounter struct {
	conns, refused, up, down atomic.Int64
}

func (m *Mediator) counter(host string) *hostCounter {
	m.countMu.Lock()
	defer m.countMu.Unlock()
	if m.counts == nil {
		m.counts = map[string]*hostCounter{}
	}
	c := m.counts[host]
	if c == nil {
		c = &hostCounter{}
		m.counts[host] = c
	}
	return c
}

// Counts returns the per-host counts so far, sorted by host.
func (m *Mediator) Counts() []HostCounts {
	m.countMu.Lock()
	defer m.countMu.Unlock()
	out := make([]HostCounts, 0, len(m.counts))
	for h, c := range m.counts {
		out = append(out, HostCounts{Host: h, Connections: c.conns.Load(), Refused: c.refused.Load(), BytesUp: c.up.Load(), BytesDown: c.down.Load()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// CountsRecord is the per-attempt counts file kept in fabd's run area.
type CountsRecord struct {
	Mission string       `json:"mission"`
	Attempt string       `json:"attempt"`
	Hosts   []HostCounts `json:"hosts"`
}

// WriteCounts writes rec to dir/{attempt}.egress.json, mode 0600, by a
// temporary file, fsync and rename.
func WriteCounts(dir string, rec CountsRecord) error {
	if dir == "" || rec.Attempt == "" || strings.ContainsAny(rec.Attempt, "/\x00") || strings.HasPrefix(rec.Attempt, ".") {
		return errors.New("egress: counts record needs a directory and a plain attempt id")
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, rec.Attempt+".egress.json")
	tmp := path + ".tmp"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// ReadCounts returns every counts record in dir for mission.
func ReadCounts(dir, mission string) ([]CountsRecord, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []CountsRecord
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".egress.json") || !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var rec CountsRecord
		if json.Unmarshal(data, &rec) == nil && rec.Mission == mission {
			out = append(out, rec)
		}
	}
	return out, nil
}
