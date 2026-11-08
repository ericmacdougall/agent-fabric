package harnesstest

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// T is the slice of testing.T the suite uses, so the suite can also run
// against a broken adapter and report violations (see Violations) instead of
// failing the test binary. That is how the suite is shown to be able to fail.
type T interface {
	Helper()
	Name() string
	TempDir() string
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
	Run(name string, f func(T)) bool
}

type tbT struct{ *testing.T }

func (t tbT) Run(name string, f func(T)) bool {
	return t.T.Run(name, func(st *testing.T) { f(tbT{st}) })
}

// probe records failures instead of failing a real test.
type probe struct {
	name string
	dir  string
	out  *[]string
}

type fatal struct{}

func (p probe) Helper()                   {}
func (p probe) Name() string              { return p.name }
func (p probe) Errorf(f string, a ...any) { *p.out = append(*p.out, p.name+": "+fmt.Sprintf(f, a...)) }
func (p probe) Fatal(a ...any)            { p.Fatalf("%s", fmt.Sprint(a...)) }
func (p probe) Fatalf(f string, a ...any) {
	p.Errorf(f, a...)
	panic(fatal{})
}
func (p probe) TempDir() string {
	d, err := os.MkdirTemp(p.dir, "probe")
	if err != nil {
		panic(err)
	}
	return d
}
func (p probe) Run(name string, f func(T)) (ok bool) {
	sub := probe{name: p.name + "/" + name, dir: p.dir, out: p.out}
	before := len(*p.out)
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, isFatal := r.(fatal); !isFatal {
					panic(r)
				}
			}
		}()
		f(sub)
	}()
	return len(*p.out) == before
}

// Violations runs the suite and returns every violation found, leaving the
// caller's test untouched. dir is a scratch directory (t.TempDir()).
func Violations(dir string, c Case) []string {
	var out []string
	run(probe{name: "conformance", dir: dir, out: &out}, c)
	return out
}

// Joined renders violations for a failure message.
func Joined(v []string) string { return strings.Join(v, "\n") }
