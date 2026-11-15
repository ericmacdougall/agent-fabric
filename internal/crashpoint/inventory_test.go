package crashpoint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// inventoryDirs are the packages whose durable writes the table must cover
// (relative to this package). cmd/fabd has no package of its own in this
// tree: fabd is `fab daemon`, in cmd/fab.
var inventoryDirs = []string{"../sched", "../store", "../records", "../credential", "../../cmd/fab"}

// writeSel are callee names that are a durable write whatever the receiver
// (fail-closed: an Exec or Commit on any value counts). writeOS and
// writeRecords are the os and records functions.
var (
	writeSel = map[string]bool{"Exec": true, "Commit": true, "CopyFrom": true, "SendBatch": true, "Execute": true,
		"MarkSettled": true, "SaveMaterial": true, "GC": true, "finishRetry": true,
		"OperatorHold": true, "ReleaseOperatorHold": true, "HoldMission": true, "PrepareLaunch": true}
	writeIdent   = map[string]bool{"writeFileAtomic": true, "writeAtomic": true, "MarkSettled": true, "SaveMaterial": true}
	writeOS      = map[string]bool{"Remove": true, "RemoveAll": true, "Rename": true, "WriteFile": true}
	writeRecords = map[string]bool{"Append": true}
	writeSQL     = regexp.MustCompile(`(?is)^\s*(INSERT|UPDATE|DELETE|WITH|MERGE|TRUNCATE)\b`)
)

// write is one durable-write call site.
type write struct {
	key  string // dir:Func:callee, the exemption key
	pos  string
	node ast.Node
}

func calleeName(call *ast.CallExpr) (recv, name string) {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return "", f.Name
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			return id.Name, f.Sel.Name
		}
		return "?", f.Sel.Name
	}
	return "", ""
}

func isWrite(call *ast.CallExpr) (string, bool) {
	recv, name := calleeName(call)
	switch {
	case recv == "" && writeIdent[name]:
		return name, true
	case recv == "os" && writeOS[name]:
		return "os." + name, true
	case recv == "records" && writeRecords[name]:
		return "records." + name, true
	case recv == "crashpoint" || recv == "os" || recv == "records" || recv == "":
		return "", false
	case writeSel[name]:
		return name, true
	case name == "QueryRow" || name == "Query":
		for _, a := range call.Args {
			if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil && writeSQL.MatchString(s) {
					return name + "(write SQL)", true
				}
			}
		}
	}
	return "", false
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// pointName is the literal name of a crashpoint.Hit, After or Around call.
func pointName(call *ast.CallExpr) (kind, name string, ok bool) {
	recv, fn := calleeName(call)
	if recv != "crashpoint" || (fn != "Hit" && fn != "After" && fn != "Around") || len(call.Args) == 0 {
		return "", "", false
	}
	lit, isLit := call.Args[0].(*ast.BasicLit)
	if !isLit || lit.Kind != token.STRING {
		return fn, "", true // a computed name is never listed
	}
	s, _ := strconv.Unquote(lit.Value)
	return fn, s, true
}

// scan returns every durable write in dir that no listed point covers, and
// every point name the code uses. A write is covered when it sits inside the
// function literal of crashpoint.Around("<listed>", ...), or its statement
// is directly between crashpoint.Hit("<listed>") and crashpoint.After of
// the same name in one block.
func scan(t *testing.T, dir string, listed map[string]bool) (uncovered []write, used map[string]bool) {
	t.Helper()
	used = map[string]bool{}
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files in %s: %v", dir, err)
	}
	short := strings.TrimLeft(strings.TrimPrefix(dir, "../"), "./")
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		type span struct{ from, to token.Pos }
		var covered []span
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				kind, name, ok := pointName(x)
				if !ok {
					return true
				}
				if name != "" {
					used[name] = true
				}
				if kind == "Around" && listed[name] && len(x.Args) == 2 {
					if fl, ok := x.Args[1].(*ast.FuncLit); ok {
						covered = append(covered, span{fl.Pos(), fl.End()})
					}
				}
			case *ast.BlockStmt:
				for i := 1; i+1 < len(x.List); i++ {
					_, before, ok1 := stmtPoint(x.List[i-1], "Hit")
					_, after, ok2 := stmtPoint(x.List[i+1], "After")
					if ok1 && ok2 && before == after && listed[before] {
						covered = append(covered, span{x.List[i].Pos(), x.List[i].End()})
					}
				}
			}
			return true
		})
		type site struct {
			call *ast.CallExpr
			key  string
		}
		var sites []site
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := funcName(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if callee, w := isWrite(call); w {
						sites = append(sites, site{call, short + ":" + fn + ":" + callee})
					}
				}
				return true
			})
		}
		within := func(n ast.Node, from, to token.Pos) bool { return n.Pos() >= from && n.End() <= to }
		for _, w := range sites {
			ok := false
			for _, c := range covered {
				if !within(w.call, c.from, c.to) {
					continue
				}
				// A write nested in another write's arguments (a release
				// record inside the release transaction) needs its own point.
				nested := false
				for _, o := range sites {
					if o.call != w.call && within(o.call, c.from, c.to) && within(w.call, o.call.Pos(), o.call.End()) {
						nested = true
					}
				}
				if !nested {
					ok = true
					break
				}
			}
			if !ok {
				p := fset.Position(w.call.Pos())
				uncovered = append(uncovered, write{key: w.key, pos: filepath.Base(p.Filename) + ":" + strconv.Itoa(p.Line), node: w.call})
			}
		}
	}
	return uncovered, used
}

func stmtPoint(s ast.Stmt, want string) (string, string, bool) {
	es, ok := s.(*ast.ExprStmt)
	if !ok {
		return "", "", false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return "", "", false
	}
	kind, name, ok := pointName(call)
	if !ok || kind != want {
		return "", "", false
	}
	return kind, name, true
}

// goodqa#15966 crash-point brief section 2: every durable write in the
// inventory packages is covered by a listed point or exempted here with a
// reason, and every listed point is in the code. A new unlisted durable
// write fails this test.
func TestEveryDurableWriteIsListed(t *testing.T) {
	listed := map[string]bool{}
	for _, p := range Table {
		if listed[p.Name] {
			t.Errorf("point %s listed twice", p.Name)
		}
		listed[p.Name] = true
	}
	used := map[string]bool{}
	var missing []string
	hitExempt := map[string]bool{}
	for _, dir := range inventoryDirs {
		unc, u := scan(t, dir, listed)
		for n := range u {
			used[n] = true
		}
		for _, w := range unc {
			if _, ok := exempt[w.key]; ok {
				hitExempt[w.key] = true
				continue
			}
			if i := strings.LastIndex(w.key, ":"); i > 0 {
				if _, ok := exempt[w.key[:i]+":*"]; ok {
					hitExempt[w.key[:i]+":*"] = true
					continue
				}
			}
			missing = append(missing, w.key+" ("+w.pos+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("durable write not covered by a listed crash point and not exempted: %s", m)
	}
	for _, p := range Table {
		if !used[p.Name] {
			t.Errorf("table point %s is not in the code", p.Name)
		}
	}
	for n := range used {
		if !listed[n] {
			t.Errorf("crash point %q in the code is not in the table", n)
		}
	}
	for k := range exempt {
		if !hitExempt[k] {
			t.Errorf("stale exemption %s: no such uncovered write", k)
		}
	}
	t.Logf("%d points, %d exemptions", len(Table), len(exempt))
}

// TestRenderTable writes the table as markdown to $CRASHPOINT_TABLE_OUT,
// for the PR evidence.
func TestRenderTable(t *testing.T) {
	out := os.Getenv("CRASHPOINT_TABLE_OUT")
	if out == "" {
		t.Skip("CRASHPOINT_TABLE_OUT not set")
	}
	if err := os.WriteFile(out, []byte(Markdown()), 0o600); err != nil {
		t.Fatal(err)
	}
}
