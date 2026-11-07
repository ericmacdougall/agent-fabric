package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// allowedImports is everything non-test code in this package may import
// besides the standard library: no fabric package but metrics, so no store,
// effects or native write API is reachable.
var allowedImports = map[string]bool{
	"fabric/internal/metrics":         true,
	"github.com/jackc/pgx/v5":         true,
	"github.com/jackc/pgx/v5/pgconn":  true,
	"github.com/jackc/pgx/v5/pgxpool": true,
	"golang.org/x/sys/unix":           true,
}

// allowedSelectors are the only names used from the database packages.
var allowedSelectors = map[string]map[string]bool{
	"pgx":     {"Tx": true, "CollectRows": true, "CollectableRow": true},
	"pgconn":  {"PgError": true, "ConnectError": true},
	"pgxpool": {"Pool": true, "ParseConfig": true, "NewWithConfig": true},
	"metrics": {"ReadOnly": true, "CollectTx": true, "Options": true, "Snapshot": true, "Label": true, "DefaultTimeout": true},
}

// txQueryMethods are the only methods called on a transaction.
var txQueryMethods = map[string]bool{"Query": true, "QueryRow": true}

// writeSQL matches write or lock keywords as whole words; OFFSET, RESET and
// ASSET do not match.
var writeSQL = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|MERGE|UPSERT|SET|LOCK|TRUNCATE|ALTER|DROP|CREATE|GRANT|REVOKE|COPY|CALL|DO|VACUUM|NOTIFY|FOR\s+(NO\s+KEY\s+UPDATE|UPDATE|SHARE|KEY\s+SHARE))\b`)

func sourceFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if len(files) < 5 {
		t.Fatalf("only %d source files parsed; the check would be vacuous", len(files))
	}
	return fset, files
}

// NEG-11 (b): structurally, the TUI cannot write. Its non-test code
//   - imports no fabric package but metrics;
//   - names only allow-listed identifiers from pgx, pgconn, pgxpool and
//     metrics (no Connect, no Conn, no Batch);
//   - calls only Query and QueryRow, and only on a pgx.Tx parameter;
//   - passes such a transaction only to its own helpers or
//     metrics.CollectTx, and uses it in no other way;
//   - touches a pool (the .Pool field) only as metrics.ReadOnly's argument,
//     and calls pgxpool.NewWithConfig only as a return value;
//   - holds no SQL literal with a write or row-lock keyword.
func TestNoDirectDBCalls(t *testing.T) {
	fset, files := sourceFiles(t)
	at := func(n ast.Node) string { return fset.Position(n.Pos()).String() }
	local := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				local[fd.Name.Name] = true
			}
		}
	}
	sqlSeen, txUses := 0, 0
	for _, f := range files {
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(p, ".") || strings.HasPrefix(p, "fabric/") {
				if !allowedImports[p] {
					t.Errorf("%s: import %q is not allowed in the read-only TUI", at(imp), p)
				}
			}
		}
		parents := map[ast.Node]ast.Node{}
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if len(stack) > 0 {
				parents[n] = stack[len(stack)-1]
			}
			stack = append(stack, n)
			return true
		})
		// Selectors on the database packages.
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok {
				if names, ok := allowedSelectors[id.Name]; ok && !names[sel.Sel.Name] {
					t.Errorf("%s: %s.%s is not allowed", at(sel), id.Name, sel.Sel.Name)
				}
				if id.Name == "pgxpool" && sel.Sel.Name == "NewWithConfig" {
					call, _ := parents[sel].(*ast.CallExpr)
					if _, ret := parents[call].(*ast.ReturnStmt); call == nil || !ret {
						t.Errorf("%s: pgxpool.NewWithConfig must be returned directly", at(sel))
					}
				}
			}
			if sel.Sel.Name == "Pool" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "pgxpool" {
					return true // the type name
				}
				call, _ := parents[sel].(*ast.CallExpr)
				if call == nil || !isSel(call.Fun, "metrics", "ReadOnly") || len(call.Args) < 2 || call.Args[1] != sel {
					t.Errorf("%s: a pool may only be passed to metrics.ReadOnly", at(sel))
				}
			}
			return true
		})
		// Transactions: every function's pgx.Tx parameters.
		ast.Inspect(f, func(n ast.Node) bool {
			var ft *ast.FuncType
			var body *ast.BlockStmt
			switch fn := n.(type) {
			case *ast.FuncDecl:
				ft, body = fn.Type, fn.Body
			case *ast.FuncLit:
				ft, body = fn.Type, fn.Body
			default:
				return true
			}
			txs := map[string]bool{}
			for _, p := range ft.Params.List {
				if isSel(p.Type, "pgx", "Tx") {
					for _, nm := range p.Names {
						txs[nm.Name] = true
					}
				}
			}
			if body == nil || len(txs) == 0 {
				return true
			}
			ast.Inspect(body, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok || !txs[id.Name] {
					return true
				}
				txUses++
				switch p := parents[id].(type) {
				case *ast.SelectorExpr: // tx.Method(...)
					if p.X == id && txQueryMethods[p.Sel.Name] {
						if _, called := parents[p].(*ast.CallExpr); called {
							return true
						}
					}
				case *ast.CallExpr: // helper(ctx, tx, ...)
					if fid, ok := p.Fun.(*ast.Ident); ok && local[fid.Name] {
						return true
					}
					if isSel(p.Fun, "metrics", "CollectTx") {
						return true
					}
				case *ast.Field: // the parameter declaration itself
					return true
				}
				t.Errorf("%s: transaction %s used outside a Query/QueryRow call or a read helper", at(id), id.Name)
				return true
			})
			return true
		})
		// Any other database method call anywhere is refused.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Exec", "Begin", "BeginTx", "BeginFunc", "Acquire", "AcquireFunc", "SendBatch", "CopyFrom", "Prepare", "Conn", "Hijack", "LargeObjects":
				t.Errorf("%s: database call .%s is not allowed", at(call), sel.Sel.Name)
			}
			return true
		})
		// SQL literals.
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if regexp.MustCompile(`(?i)\bSELECT\b`).MatchString(s) {
				sqlSeen++
				if m := writeSQL.FindString(s); m != "" {
					t.Errorf("%s: SQL holds %q", at(lit), m)
				}
			}
			return true
		})
	}
	if sqlSeen < 8 || txUses < 10 {
		t.Fatalf("scanned %d SQL literals and %d transaction uses; the check would be vacuous", sqlSeen, txUses)
	}
	for _, ok := range []string{"OFFSET 5", "reset", "asset"} {
		if writeSQL.MatchString("SELECT " + ok) {
			t.Fatalf("keyword scan false positive on %q", ok)
		}
	}
	for _, bad := range []string{"select 1 for update", "SELECT 1 FOR NO KEY UPDATE", "select 1 for share", "SET x = 1"} {
		if !writeSQL.MatchString(bad) {
			t.Fatalf("keyword scan misses %q", bad)
		}
	}
}

func isSel(e ast.Expr, pkg, name string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == pkg && s.Sel.Name == name
}
