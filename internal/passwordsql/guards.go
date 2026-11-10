package passwordsql

import (
	"context"
	"reflect"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// No unknown module or sink is switched off to make the operation pass.
func Approved(version, destination, shared, session, local, path string, extensions []string) bool {
	if version != "160015" || destination != "stderr" || session != "" || local != "" || path != "$libdir" {
		return false
	}
	shared = strings.TrimSpace(shared)
	if shared != "" && shared != "pg_stat_statements" {
		return false
	}
	ext := append([]string(nil), extensions...)
	sort.Strings(ext)
	if reflect.DeepEqual(ext, []string{"plpgsql=1.0"}) {
		return shared == ""
	}
	return shared == "pg_stat_statements" && reflect.DeepEqual(ext, []string{"pg_stat_statements=1.10", "plpgsql=1.0"})
}

var settings = []struct{ name, value string }{
	{"track_activities", "off"}, {"log_statement", "none"}, {"log_min_error_statement", "panic"}, {"log_min_duration_statement", "-1"},
	{"log_error_verbosity", "terse"}, {"log_min_messages", "panic"}, {"log_parameter_max_length", "0"}, {"log_parameter_max_length_on_error", "0"},
	{"debug_print_parse", "off"}, {"debug_print_rewritten", "off"}, {"debug_print_plan", "off"},
}

type Protection struct {
	names, before []string
	stats         bool
}

func snapshot(ctx context.Context, q Queryer, names []string) ([]string, error) {
	var values []string
	err := q.QueryRow(ctx, `SELECT ARRAY(SELECT COALESCE(pg_catalog.current_setting(n, true), '') FROM pg_catalog.unnest($1::text[]) WITH ORDINALITY AS x(n,i) ORDER BY i)`, pgx.QueryExecModeExec, names).Scan(&values)
	if err != nil {
		return nil, Safe(err)
	}
	return values, nil
}
func Protect(ctx context.Context, q Queryer) (Protection, error) {
	var p Protection
	var version, dest, shared, session, local, path string
	var extensions []string
	err := q.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version_num'), pg_catalog.current_setting('log_destination'), pg_catalog.current_setting('shared_preload_libraries'), pg_catalog.current_setting('session_preload_libraries'), pg_catalog.current_setting('local_preload_libraries'), pg_catalog.current_setting('dynamic_library_path'), ARRAY(SELECT extname || '=' || extversion FROM pg_catalog.pg_extension ORDER BY extname)`).Scan(&version, &dest, &shared, &session, &local, &path, &extensions)
	if err != nil {
		return p, Safe(err)
	}
	if !Approved(version, dest, shared, session, local, path, extensions) {
		return p, ErrRefused
	}
	p.stats = shared == "pg_stat_statements"
	for _, s := range settings {
		p.names = append(p.names, s.name)
	}
	if p.stats {
		p.names = append(p.names, "pg_stat_statements.track", "pg_stat_statements.track_utility")
	}
	p.before, err = snapshot(ctx, q, p.names)
	if err != nil {
		return p, err
	}
	for _, s := range settings {
		// Fixed compile-time setting names/values only, before any secret Bind.
		if _, err := q.Exec(ctx, "SET LOCAL "+s.name+" = '"+s.value+"'"); err != nil {
			return p, Safe(err)
		}
	}
	if p.stats {
		for _, s := range []string{"SET LOCAL pg_stat_statements.track = 'none'", "SET LOCAL pg_stat_statements.track_utility = 'off'"} {
			if _, err := q.Exec(ctx, s); err != nil {
				return p, Safe(err)
			}
		}
	}
	got, err := snapshot(ctx, q, p.names)
	if err != nil {
		return p, err
	}
	want := []string{}
	for _, s := range settings {
		want = append(want, s.value)
	}
	if p.stats {
		want = append(want, "none", "off")
	}
	if !reflect.DeepEqual(got, want) {
		return p, ErrRefused
	}
	return p, nil
}
func (p Protection) Restored(ctx context.Context, q Queryer) error {
	if len(p.names) == 0 {
		return nil
	}
	got, err := snapshot(ctx, q, p.names)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, p.before) {
		return ErrRefused
	}
	return nil
}
