package passwordsql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type row struct {
	values []any
	err    error
}

func (r row) Scan(dst ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dst) != len(r.values) {
		return errors.New("wrong guard shape")
	}
	for i, v := range r.values {
		switch p := dst[i].(type) {
		case *string:
			*p = v.(string)
		case *[]string:
			*p = v.([]string)
		case *bool:
			*p = v.(bool)
		default:
			return errors.New("wrong guard type")
		}
	}
	return nil
}

type guards struct {
	values            map[string]string
	deny, ineffective string
	collision         bool
	secrets           int
}

func (g *guards) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "server_version_num") {
		return row{values: []any{"160015", "stderr", "", "", "", "$libdir", []string{"plpgsql=1.0"}}}
	}
	if strings.Contains(sql, "WITH ORDINALITY") {
		names := args[1].([]string)
		v := []string{}
		for _, n := range names {
			v = append(v, g.values[n])
		}
		return row{values: []any{v}}
	}
	if len(args) > 2 && args[2] == "PUBLIC_MARKER" {
		g.secrets++
	}
	if strings.Contains(sql, "IS NULL AND NOT EXISTS") {
		return row{values: []any{!g.collision}}
	}
	return row{values: []any{true}}
}
func (g *guards) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	for _, s := range settings {
		if strings.Contains(sql, "SET LOCAL "+s.name+" =") {
			if s.name == g.deny {
				return pgconn.CommandTag{}, &pgconn.PgError{Code: "42501", Message: "unsafe raw context"}
			}
			if s.name != g.ineffective {
				g.values[s.name] = s.value
			}
		}
	}
	return pgconn.NewCommandTag("SET"), nil
}
func newGuards() *guards {
	g := &guards{values: map[string]string{}}
	for _, s := range settings {
		g.values[s.name] = "original"
	}
	return g
}
func TestProtectionAndHandoffFailuresBeforeSecret(t *testing.T) {
	for _, name := range []string{"track_activities", "log_error_verbosity", "log_parameter_max_length", "debug_print_parse"} {
		g := newGuards()
		g.deny = name
		if _, err := Protect(context.Background(), g); err == nil {
			t.Error("protection failure accepted")
		}
		if g.secrets != 0 {
			t.Error("secret transmitted before protection")
		}
		g = newGuards()
		g.ineffective = name
		if _, err := Protect(context.Background(), g); err == nil {
			t.Error("ineffective protection accepted")
		}
	}
	g := newGuards()
	p, err := Protect(context.Background(), g)
	if err != nil {
		t.Fatal("fixed protections refused")
	}
	for _, s := range settings {
		g.values[s.name] = "original"
	}
	if err := p.Restored(context.Background(), g); err != nil {
		t.Fatal("restoration mismatch")
	}
	g.values["track_activities"] = "off"
	if err := p.Restored(context.Background(), g); err == nil {
		t.Error("bad restoration accepted")
	}
	key, _ := NewKey()
	g.collision = true
	if err := Preclaim(context.Background(), g, key); err == nil {
		t.Error("colliding handoff accepted")
	}
	if g.secrets != 0 {
		t.Error("secret transmitted on collision")
	}
	if err := Set(context.Background(), g, key, "fixture", "bad\x00password"); err == nil {
		t.Error("invalid password accepted")
	}
}
