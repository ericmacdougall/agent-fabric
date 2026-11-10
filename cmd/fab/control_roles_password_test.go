package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlRolesLoadedPasswordsNeedConfirmation(t *testing.T) {
	for role := 0; role < 3; role++ {
		for _, value := range []string{"", " \t\r\n"} {
			files := []string{"", "", ""}
			for i := range files {
				files[i] = filepath.Join(t.TempDir(), "password")
				content := "synthetic-only-" + strings.Repeat("X", 24)
				if i == role {
					content = value
				}
				if err := os.WriteFile(files[i], []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadControlPasswords(files[0], files[1], files[2], false); exitCode(err) != 3 {
				t.Error("empty effective password did not require confirmation")
			}
			pw, err := loadControlPasswords(files[0], files[1], files[2], true)
			if err != nil {
				t.Fatal("explicit NOLOGIN was refused")
			}
			if []string{pw.Export, pw.Ingest, pw.Service}[role] != "" {
				t.Error("whitespace did not become NOLOGIN")
			}
			// A refused CLI call must complete before withStore even considers FABRIC_DSN.
			t.Setenv("FABRIC_DSN", "")
			if err := controlRolesMain("test", files[0], files[1], files[2], false, false, false, false); exitCode(err) != 3 {
				t.Error("CLI attempted DB access before effective password validation")
			}
		}
	}
	if _, err := loadControlPasswords("", "", "", false); exitCode(err) != 3 {
		t.Error("missing paths accepted without confirmation")
	}
	if _, err := loadControlPasswords(filepath.Join(t.TempDir(), "absent"), "", "", true); err == nil {
		t.Error("unreadable file accepted")
	}
}
