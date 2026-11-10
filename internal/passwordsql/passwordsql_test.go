package passwordsql

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A PostgreSQL wire double, not a database or authentication acceptance claim.
// It observes Parse/Query/Bind separately and never prints fixture values.
func wire(t *testing.T, marker string, failUtility ...bool) (*pgx.Conn, func() (int, int)) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("wire listener")
	}
	t.Cleanup(func() { listener.Close() })
	var mu sync.Mutex
	sqlSecrets, bindSecrets := 0, 0
	go func() {
		server, err := listener.Accept()
		if err != nil {
			return
		}
		defer server.Close()
		b := pgproto3.NewBackend(server, server)
		if _, err := b.ReceiveStartupMessage(); err != nil {
			return
		}
		b.Send(&pgproto3.AuthenticationOk{})
		b.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
		b.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
		b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if b.Flush() != nil {
			return
		}
		selectResult := false
		desc := func() {
			b.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("ok"), DataTypeOID: 16, DataTypeSize: 1, TypeModifier: -1}}})
		}
		for {
			msg, err := b.Receive()
			if err != nil {
				return
			}
			switch v := msg.(type) {
			case *pgproto3.Query:
				if !strings.HasPrefix(v.String, "SELECT") && len(failUtility) > 0 && failUtility[0] {
					b.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "XX000", Message: marker, InternalQuery: marker, Where: marker})
					b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
					break
				}
				mu.Lock()
				if strings.Contains(v.String, marker) {
					sqlSecrets++
				}
				mu.Unlock()
				if strings.HasPrefix(v.String, "SELECT") {
					desc()
					b.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("t")}})
					b.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
				} else {
					b.Send(&pgproto3.CommandComplete{CommandTag: []byte("DO")})
				}
				b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			case *pgproto3.Parse:
				mu.Lock()
				if strings.Contains(v.Query, marker) {
					sqlSecrets++
				}
				mu.Unlock()
				selectResult = strings.HasPrefix(v.Query, "SELECT")
				b.Send(&pgproto3.ParseComplete{})
			case *pgproto3.Bind:
				mu.Lock()
				for _, p := range v.Parameters {
					if strings.Contains(string(p), marker) {
						bindSecrets++
					}
				}
				mu.Unlock()
				b.Send(&pgproto3.BindComplete{})
			case *pgproto3.Describe:
				if selectResult {
					desc()
				} else {
					b.Send(&pgproto3.NoData{})
				}
			case *pgproto3.Execute:
				if !selectResult && len(failUtility) > 0 && failUtility[0] {
					b.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "XX000", Message: marker, InternalQuery: marker, Where: marker})
					break
				}
				if selectResult {
					b.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("t")}})
					b.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
				} else {
					b.Send(&pgproto3.CommandComplete{CommandTag: []byte("DO")})
				}
			case *pgproto3.Sync:
				b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			case *pgproto3.Terminate:
				return
			default:
				return
			}
			if b.Flush() != nil {
				return
			}
		}
	}()
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=127.0.0.1 port=%d user=fixture database=fixture sslmode=disable passfile=/dev/null", listener.Addr().(*net.TCPAddr).Port))
	if err != nil {
		t.Fatal("wire config")
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("wire connect")
	}
	t.Cleanup(func() { c.Close(ctx) })
	return c, func() (int, int) { mu.Lock(); defer mu.Unlock(); return sqlSecrets, bindSecrets }
}
func TestWireExtendedPasswordBoundary(t *testing.T) {
	marker := "PUBLIC_FIXTURE_" + strings.Repeat("Q", 20)
	old, oldCounts := wire(t, marker)
	if _, err := old.Exec(context.Background(), "ALTER ROLE fixture PASSWORD '"+marker+"'"); err != nil {
		t.Fatal("old control execution")
	}
	a, b := oldCounts()
	if a != 1 || b != 0 {
		t.Fatal("old literal control did not prove exposure")
	}
	c, counts := wire(t, marker)
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := Set(context.Background(), c, key, "fixture", marker); err != nil {
		t.Fatal("protected wire execution failed")
	}
	a, b = counts()
	if a != 0 || b != 1 {
		t.Fatal("password entered SQL or failed to use extended Bind")
	}
}
func TestBuildersAndSafeErrors(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	q, err := AlterSQL("fixture", key)
	if err != nil || !strings.Contains(q, "pg_catalog.format('ALTER ROLE %I NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION NOINHERIT LOGIN PASSWORD %L'") {
		t.Fatal("builder lost single escaped utility")
	}
	if strings.Contains(BindSQL, "SELECT pg_catalog.set_config($1, $2, true)") && !strings.HasSuffix(BindSQL, "IS NOT NULL") {
		t.Fatal("password-valued result")
	}
	for _, v := range []string{"bad'role", "bad.role", "", strings.Repeat("x", 64)} {
		if _, err := AlterSQL(v, key); err == nil {
			t.Error("invalid role accepted")
		}
	}
	if Valid("fixture", "bad\x00value") || Valid("fixture", string([]byte{255})) {
		t.Error("invalid password accepted")
	}
	secret := "PUBLIC_FIXTURE_MARKER"
	err = Safe(&pgconn.PgError{Code: "XX000", Message: secret, Detail: secret, InternalQuery: secret, Where: secret})
	if err.Error() != "sqlstate XX000" || strings.Contains(err.Error(), secret) {
		t.Fatal("raw error retained")
	}
}
func TestCallbackRefusals(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("host=policy.fixture.invalid user=fixture database=fixture sslmode=disable passfile=/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DedicatedConfig(cfg); err != nil {
		t.Fatalf("stock factory refused: %v", err)
	}
	variants := []func(*pgxpool.Config){func(p *pgxpool.Config) { p.AfterConnect = func(context.Context, *pgx.Conn) error { return nil } }, func(p *pgxpool.Config) { p.ConnConfig.OnNotice = func(*pgconn.PgConn, *pgconn.Notice) {} }, func(p *pgxpool.Config) { p.ConnConfig.OnNotification = func(*pgconn.PgConn, *pgconn.Notification) {} }, func(p *pgxpool.Config) {
		p.ConnConfig.BuildFrontend = func(io.Reader, io.Writer) *pgproto3.Frontend { return nil }
	}, func(p *pgxpool.Config) {
		p.ConnConfig.AfterConnect = func(context.Context, *pgconn.PgConn) error { return nil }
	}}
	for _, v := range variants {
		c := cfg.Copy()
		v(c)
		if _, err := DedicatedConfig(c); err == nil {
			t.Error("unsupported callback accepted")
		}
	}
}
func TestApprovedRuntimeRefusesUnknownHooksAndSinks(t *testing.T) {
	if !Approved("160015", "stderr", "", "", "", "$libdir", []string{"plpgsql=1.0"}) {
		t.Fatal("stock profile refused")
	}
	if !Approved("160015", "stderr", "pg_stat_statements", "", "", "$libdir", []string{"plpgsql=1.0", "pg_stat_statements=1.10"}) {
		t.Fatal("approved statistics profile refused")
	}
	for _, sink := range []string{"csvlog", "jsonlog", "stderr,csvlog"} {
		if Approved("160015", sink, "", "", "", "$libdir", []string{"plpgsql=1.0"}) {
			t.Error("unsafe destination accepted")
		}
	}
	for _, module := range []string{"auto_explain", "pgaudit", "pg_stat_statements,auto_explain"} {
		if Approved("160015", "stderr", module, "", "", "$libdir", []string{"plpgsql=1.0"}) {
			t.Error("unknown module accepted")
		}
	}
	if Approved("160015", "stderr", "", "unknown", "", "$libdir", []string{"plpgsql=1.0"}) || Approved("160015", "stderr", "", "", "", "$libdir", []string{"plpgsql=1.0", "unknown=1.0"}) {
		t.Error("unknown hook profile accepted")
	}
}

func TestFrontendSensitiveErrorIsReducedToSQLState(t *testing.T) {
	marker := "PUBLIC_FIXTURE_" + strings.Repeat("E", 20)
	c, _ := wire(t, marker, true)
	key, _ := NewKey()
	err := Set(context.Background(), c, key, "fixture", marker)
	if err == nil || err.Error() != "sqlstate XX000" || strings.Contains(err.Error(), marker) {
		t.Fatal("frontend detail/context escaped safe error boundary")
	}
	if err := c.Close(context.Background()); err != nil || !c.IsClosed() {
		t.Fatal("dedicated wire connection not closed")
	}
}
