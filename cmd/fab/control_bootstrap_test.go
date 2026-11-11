package main

import (
	"context"
	"errors"
	"testing"

	"fabric/internal/control"
	"fabric/internal/storetest"
)

func TestControlBootstrapDispatch(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if _, err := st.Pool().Exec(ctx, "DROP SCHEMA IF EXISTS control CASCADE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Pool().Exec(ctx, "DROP SCHEMA IF EXISTS control CASCADE") }) //nolint:errcheck
	t.Setenv("FABRIC_DSN", storetest.DSN)
	var before int
	if err := st.Pool().QueryRow(ctx, "SELECT count(*) FROM fabric_schema").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := controlMain([]string{"bootstrap"}); err != nil {
			t.Fatalf("bootstrap call%d: %v", i, err)
		}
	}
	var versions, after int
	var hash string
	if err := st.Pool().QueryRow(ctx, "SELECT count(*), max(sha256) FROM control.bootstrap_ledger").Scan(&versions, &hash); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool().QueryRow(ctx, "SELECT count(*) FROM fabric_schema").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if versions != control.SchemaVersion() || hash != control.SchemaSHA256() || before != after {
		t.Fatalf("versions=%d hash_matches=%v schema_count=%d->%d", versions, hash == control.SchemaSHA256(), before, after)
	}
}

func TestControlBootstrapRefusesAppAndRunner(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	if _, err := st.Pool().Exec(ctx, "DROP SCHEMA IF EXISTS control CASCADE"); err != nil {
		t.Fatal(err)
	}
	app, runner := storetest.AppRunnerDSNs(t, st)
	for name, dsn := range map[string]string{"app": app, "runner": runner} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("FABRIC_DSN", dsn)
			if err := controlMain([]string{"bootstrap"}); err == nil {
				t.Fatal("non-owner bootstrap succeeded")
			} else {
				var usage usageError
				if errors.As(err, &usage) {
					t.Fatal("dispatcher did not reach the bootstrap permission check")
				}
			}
			var absent bool
			if err := st.Pool().QueryRow(ctx, "SELECT to_regnamespace('control') IS NULL").Scan(&absent); err != nil || !absent {
				t.Fatalf("non-owner left a control schema: absent=%v err=%v", absent, err)
			}
		})
	}
}

func TestControlBootstrapRejectsArguments(t *testing.T) {
	t.Setenv("FABRIC_DSN", "")
	var usage usageError
	if err := controlMain([]string{"bootstrap", "unexpected"}); !errors.As(err, &usage) {
		t.Fatalf("expected usage refusal, got%v", err)
	}
}
