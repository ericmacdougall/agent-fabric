// Package passwordsql keeps role passwords in extended-protocol Bind messages.
// It assumes the separately reviewed stock-server and crash-artifact boundary.
package passwordsql

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrRefused = errors.New("password session policy or verification refused")
var roleRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
var keyRE = regexp.MustCompile(`^fabric_password_[0-9a-f]{32}\.handoff$`)
var stateRE = regexp.MustCompile(`^[A-Z0-9]{5}$`)

type stateError string

func (s stateError) Error() string    { return "sqlstate " + string(s) }
func (s stateError) SQLState() string { return string(s) }

// Safe never retains a PgError, its internal query/context, Bind args or DSN.
func Safe(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrRefused) {
		return ErrRefused
	}
	var pe interface{ SQLState() string }
	if errors.As(err, &pe) && stateRE.MatchString(pe.SQLState()) {
		return stateError(pe.SQLState())
	}
	return errors.New("database operation failed")
}
func Valid(role, password string) bool {
	return roleRE.MatchString(role) && utf8.ValidString(password) && !strings.ContainsRune(password, 0)
}
func NewKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", ErrRefused
	}
	return "fabric_password_" + hex.EncodeToString(b[:]) + ".handoff", nil
}

// BindSQL returns a boolean, never set_config's password-valued text result.
const BindSQL = `SELECT pg_catalog.set_config($1, $2, true) IS NOT NULL`

func ControlAlterSQL(role, key string) (string, error) { return alter(role, key, false) }
func StoreAlterSQL(role, key string) (string, error)   { return alter(role, key, true) }
func alter(role, key string, store bool) (string, error) {
	if !roleRE.MatchString(role) || !keyRE.MatchString(key) {
		return "", ErrRefused
	}
	if store {
		return `DO $fabric_password$ BEGIN EXECUTE pg_catalog.format('ALTER ROLE %I NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION LOGIN PASSWORD %L', '` + role + `', pg_catalog.current_setting('` + key + `')); END $fabric_password$ LANGUAGE plpgsql`, nil
	}
	return `DO $fabric_password$ BEGIN EXECUTE pg_catalog.format('ALTER ROLE %I NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION NOINHERIT LOGIN PASSWORD %L', '` + role + `', pg_catalog.current_setting('` + key + `')); END $fabric_password$ LANGUAGE plpgsql`, nil
}

type Queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func boolean(ctx context.Context, q Queryer, sql string, args ...any) error {
	var ok bool
	args = append([]any{pgx.QueryExecModeExec}, args...)
	if err := q.QueryRow(ctx, sql, args...).Scan(&ok); err != nil {
		return Safe(err)
	}
	if !ok {
		return ErrRefused
	}
	return nil
}
func Preclaim(ctx context.Context, q Queryer, key string) error {
	if !keyRE.MatchString(key) {
		return ErrRefused
	}
	if err := boolean(ctx, q, `SELECT pg_catalog.current_setting($1, true) IS NULL AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_settings WHERE name=$1)`, key); err != nil {
		return err
	}
	return Clear(ctx, q, key)
}
func Clear(ctx context.Context, q Queryer, key string) error {
	if !keyRE.MatchString(key) {
		return ErrRefused
	}
	if err := boolean(ctx, q, BindSQL, key, ""); err != nil {
		return err
	}
	return Empty(ctx, q, key)
}
func Empty(ctx context.Context, q Queryer, key string) error {
	return boolean(ctx, q, `SELECT COALESCE(pg_catalog.current_setting($1, true), '') = ''`, key)
}
func Set(ctx context.Context, q Queryer, key, role, password string) error {
	return set(ctx, q, key, role, password, false)
}
func SetStore(ctx context.Context, q Queryer, key, role, password string) error {
	return set(ctx, q, key, role, password, true)
}
func set(ctx context.Context, q Queryer, key, role, password string, store bool) error {
	if !Valid(role, password) || password == "" {
		return ErrRefused
	}
	sql, err := alter(role, key, store)
	if err != nil {
		return err
	}
	if err := boolean(ctx, q, BindSQL, key, password); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, sql, pgx.QueryExecModeExec); err != nil {
		return Safe(err)
	}
	return Clear(ctx, q, key)
}

func functionName(f any) string {
	v := reflect.ValueOf(f)
	if !v.IsValid() || v.IsNil() {
		return ""
	}
	return runtime.FuncForPC(v.Pointer()).Name()
}

// DedicatedConfig accepts only the reviewed Store.Open stock pgx factory. It
// refuses pool policy hooks rather than silently omitting them on a fresh conn.
// Stock callback identity is a version-pinned source check, not a sandbox for
// arbitrary caller-owned function closures or a malicious Go process.
func DedicatedConfig(p *pgxpool.Config) (*pgx.ConnConfig, error) {
	if p == nil || p.ConnConfig == nil || p.BeforeConnect != nil || p.AfterConnect != nil || p.BeforeAcquire != nil || p.PrepareConn != nil || p.AfterRelease != nil || p.BeforeClose != nil || p.ShouldPing != nil {
		return nil, ErrRefused
	}
	c := p.ConnConfig.Copy()
	if c.Tracer != nil || c.OnNotice != nil || c.OnNotification != nil || c.AfterConnect != nil || c.AfterNetConnect != nil || c.OAuthTokenProvider != nil {
		return nil, ErrRefused
	}
	base := "github.com/jackc/pgx/v5/pgconn."
	for f, want := range map[string]string{functionName(c.BuildFrontend): base + "ParseConfigWithOptions.func1", functionName(c.BuildContextWatcherHandler): base + "ParseConfigWithOptions.func2", functionName(c.OnPgError): base + "ParseConfigWithOptions.func3"} {
		if f != want {
			return nil, ErrRefused
		}
	}
	dial := functionName(c.DialFunc)
	if dial != "net.(*Dialer).DialContext-fm" && dial != base+"makeConnectTimeoutDialFunc.func1" {
		return nil, ErrRefused
	}
	if functionName(c.LookupFunc) != "net.(*Resolver).LookupHost-fm" {
		return nil, ErrRefused
	}
	validate := functionName(c.ValidateConnect)
	if validate != "" && validate != base+"ValidateConnectTargetSessionAttrsReadWrite" && validate != base+"ValidateConnectTargetSessionAttrsReadOnly" && validate != base+"ValidateConnectTargetSessionAttrsPrimary" && validate != base+"ValidateConnectTargetSessionAttrsStandby" && validate != base+"ValidateConnectTargetSessionAttrsPreferStandby" {
		return nil, ErrRefused
	}
	return c, nil // TLS/authentication/fallback/validation defaults stay intact.
}

// Connect preserves the reviewed stock connection policy on a new backend.
func Connect(ctx context.Context, p *pgxpool.Config) (*pgx.Conn, error) {
	cfg, err := DedicatedConfig(p)
	if err != nil {
		return nil, err
	}
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, Safe(err)
	}
	return c, nil
}

// Close finishes only this dedicated backend. It never returns it to a pool,
// and does not infer absence of writes from rollback/transport failure.
func Close(ctx context.Context, c *pgx.Conn, tx pgx.Tx, p Protection, key string, sensitive, committed bool, operationErr error) error {
	if !committed && operationErr == nil {
		operationErr = errors.New("commit outcome unconfirmed")
	}
	if tx != nil && !committed {
		tx.Rollback(ctx)
	} //nolint:errcheck
	if sensitive && !c.IsClosed() {
		cleanupErr := p.Restored(ctx, c)
		if key != "" && cleanupErr == nil {
			cleanupErr = Empty(ctx, c, key)
		}
		if cleanupErr != nil && operationErr == nil {
			if committed {
				operationErr = errors.New("roles committed; session cleanup could not be verified; dedicated connection closed")
			} else {
				operationErr = errors.New("session cleanup could not be verified; dedicated connection closed")
			}
		}
	}
	if err := c.Close(ctx); err != nil && operationErr == nil {
		if committed {
			return errors.New("roles committed; dedicated connection close failed")
		}
		return errors.New("dedicated connection close failed")
	}
	return operationErr
}
