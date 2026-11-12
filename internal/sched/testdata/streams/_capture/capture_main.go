//go:build settlegolden

// Capture program for goodqa#15373 AC-5 goldens. Run ONLY in a tree of
// bd56178 code with zz_settle_export.go added to internal/sched:
//
//	cd internal/sched && go run -tags settlegolden ../../capture/
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"fabric/internal/domain"
	"fabric/internal/sched"
)

func main() {

	cases, err := os.ReadDir("testdata/streams")
	if err != nil {
		panic(err)
	}
	for _, c := range cases {
		src := filepath.Join("testdata/streams", c.Name())
		b, err := os.ReadFile(filepath.Join(src, "exit"))
		if err != nil {
			panic(err)
		}
		code, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		run, _ := os.MkdirTemp("", "gold-")
		fi, err := os.Stat(filepath.Join(src, "stream.jsonl"))
		if err != nil {
			panic(err)
		}
		if fi.IsDir() {
			os.Mkdir(filepath.Join(run, "stream.jsonl"), 0o755)
		} else {
			s, _ := os.ReadFile(filepath.Join(src, "stream.jsonl"))
			os.WriteFile(filepath.Join(run, "stream.jsonl"), s, 0o644)
		}
		cmd, err := sched.SettleCommandForGolden(domain.MissionID("m_golden"), domain.AttemptID("a_golden"), 7, run, code)
		out := map[string]any{"command": cmd, "error": nil, "usage": nil}
		if err != nil {
			out["error"] = err.Error()
		}
		if u, err := os.ReadFile(filepath.Join(run, "usage.json")); err == nil {
			out["usage"] = string(u)
		}
		g, _ := json.MarshalIndent(out, "", "  ")
		if err := os.WriteFile(filepath.Join(src, "golden.json"), append(g, '\n'), 0o644); err != nil {
			panic(err)
		}
		fmt.Printf("%s: %s\n", c.Name(), g)
	}
}
