package corpus

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// NEG-9 (spec v4.3): the committed corpus and vectors are synthetic with no
// live ids, tickets, hostnames, paths or tokens. This scans every testdata
// file of the F1a packages; it fails on any such pattern, and a planted
// pattern proves the scan can fail.
var redactions = map[string]*regexp.Regexp{
	"home path":  regexp.MustCompile(`/(home|Users)/[A-Za-z0-9_.-]+`),
	"ticket":     regexp.MustCompile(`(?i)goodqa\s*#\s*\d+`),
	"host":       regexp.MustCompile(`(?i)\b(t2-dev|dev2|t2-proxmox|tailscale|\.ts\.net)\b`),
	"token":      regexp.MustCompile(`(sk-[A-Za-z0-9]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16})`),
	"dsn":        regexp.MustCompile(`(?i)postgres(ql)?://`),
	"ip address": regexp.MustCompile(`\b(\d{1,3}\.){3}\d{1,3}\b`),
	"email":      regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[a-z]{2,}`),
}

func scanText(b []byte) []string {
	var hits []string
	for name, re := range redactions {
		if re.Match(b) {
			hits = append(hits, name)
		}
	}
	return hits
}

func TestTestdataIsRedacted(t *testing.T) {
	var n int
	for _, dir := range []string{"testdata", "../../envelope/testdata"} {
		err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			n++
			if hits := scanText(b); len(hits) > 0 {
				t.Errorf("%s carries %v", p, hits)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n == 0 {
		t.Fatal("no testdata files scanned")
	}
}

func TestRedactionScanCanFail(t *testing.T) {
	for name, sample := range map[string]string{
		"home path":  "/home/someone/x",
		"ticket":     "GoodQA #12345",
		"host":       "ssh t2-dev",
		"token":      "ghp_abcdefghijklmnopqrstuvwxyz",
		"dsn":        "postgres://u@h/db",
		"ip address": "10.1.2.3",
		"email":      "a@b.example",
	} {
		hits := scanText([]byte(sample))
		found := false
		for _, h := range hits {
			found = found || h == name
		}
		if !found {
			t.Errorf("scan missed a planted %s (%q): hits %v", name, sample, hits)
		}
	}
}
