// Package corpus builds the synthetic v1 event logs that pin F1a's replay
// parity (goodqa#15372): every JSON field of domain.Event is non-zero in the
// corpus, `revision` differs from `generation`, and each known event kind
// appears. The logs are synthetic: no live ids, tickets, hosts or tokens.
//
// The package depends only on domain and the reducer's pure Apply and Digest,
// so the same source runs at the pre-change commit to produce the golden
// digests (testdata/parity-golden.json) that the new reader must reproduce.
package corpus

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"fabric/internal/domain"
	"fabric/internal/reducer"
)

// fill sets every field of the struct v points to a deterministic non-zero
// value: strings "x-<n>", integers n+1, bools true, one-element slices, and
// pointers to filled values. n is a counter so every field differs.
func fill(v reflect.Value, n *int) {
	switch v.Kind() {
	case reflect.Ptr:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), n)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).Tag.Get("json") == "-" || !v.Field(i).CanSet() {
				continue
			}
			fill(v.Field(i), n)
		}
	case reflect.String:
		*n++
		v.SetString(fmt.Sprintf("x-%d", *n))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		*n++
		v.SetInt(int64(*n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), n)
		v.Set(s)
	}
}

// Event returns a domain.Event of the given kind with every JSON field set,
// for mission and seq. Revision and Generation always differ.
func Event(kind domain.EventKind, mission domain.MissionID, seq int64, salt int) domain.Event {
	var e domain.Event
	n := salt * 100
	fill(reflect.ValueOf(&e).Elem(), &n)
	e.Kind, e.Mission, e.Seq = kind, mission, seq
	e.Revision, e.Generation = 3, 7
	return e
}

// prelude creates the maps the reducer's Apply assumes exist (attempts and
// effects), so a kind that refers to them folds without panicking.
func prelude(id domain.MissionID) []domain.Event {
	return []domain.Event{
		{Kind: domain.EvMissionAdmitted, Mission: id, Seq: 1},
		Event(domain.EvAttemptStarted, id, 2, 90),
		Event(domain.EvEffectSubmitted, id, 3, 91),
	}
}

// Logs returns the corpus: one mission holding every known kind with every
// field set after a prelude, and one mission per kind holding it alone after
// the same prelude. The order of the kinds is sorted, so the corpus is stable.
func Logs() map[domain.MissionID][]domain.Event {
	logs := map[domain.MissionID][]domain.Event{}
	kinds := domain.KnownEventKinds()
	sort.Strings(kinds)
	all := domain.MissionID("m_corpus_all")
	logs[all] = prelude(all)
	for i, k := range kinds {
		logs[all] = append(logs[all], Event(domain.EventKind(k), all, int64(i+4), i+1))
	}
	for i, k := range kinds {
		id := domain.MissionID("m_corpus_" + strings.ToLower(k))
		logs[id] = append(prelude(id), Event(domain.EventKind(k), id, 4, i+50))
	}
	return logs
}

// Fold folds a log from the zero state with the reducer's pure Apply, the way
// the authority did before F1a (no registry check, no view).
func Fold(id domain.MissionID, events []domain.Event) domain.Mission {
	m := domain.NewMission(id)
	for _, e := range events {
		m = reducer.Apply(m, e)
	}
	return m
}

// Digests returns mission -> digest of the folded state.
func Digests() map[domain.MissionID]string {
	out := map[domain.MissionID]string{}
	for id, ev := range Logs() {
		out[id] = reducer.Digest(Fold(id, ev))
	}
	return out
}
