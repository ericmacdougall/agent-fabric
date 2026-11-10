package main

import (
	"context"
	"errors"
	"fmt"

	"fabric/internal/control"
	"fabric/internal/store"
)

// controlRolesMain runs `fab db roles --control-only` (goodqa#15623): it applies,
// verifies or removes the three Control Room roles, never the fabric roles. The
// password files are read here and never printed.
func controlRolesMain(prefix, exportFile, ingestFile, serviceFile string, verify, remove, confirm, allowNologin bool) error {
	if confirm && !remove {
		return usageError{errors.New("--confirm needs --remove")}
	}
	if verify && remove {
		return usageError{errors.New("--verify and --remove are separate runs")}
	}
	if prefix == "" {
		return usageError{errors.New("fab db roles --control-only needs an explicit --prefix (fabric_svc on live): there is no default")}
	}
	r := control.Roles{Prefix: prefix}
	switch {
	case verify:
		return withStore(func(ctx context.Context, st *store.Store) error {
			problems, notes, err := control.VerifyRoles(ctx, st.Pool(), r)
			if err != nil {
				return err
			}
			for _, n := range notes {
				fmt.Println("note:", n)
			}
			for _, p := range problems {
				fmt.Println("PROBLEM:", p)
			}
			if len(problems) > 0 {
				return control.ErrVerify
			}
			fmt.Println("control roles ok")
			return nil
		})
	case remove:
		return withStore(func(ctx context.Context, st *store.Store) error {
			plan, err := control.RemoveRoles(ctx, st.Pool(), r, confirm)
			for _, p := range plan {
				fmt.Println("plan:", p)
			}
			if err != nil {
				return err
			}
			if !confirm {
				fmt.Println("nothing changed: run again with --confirm to apply the plan")
			}
			return nil
		})
	}
	pw, err := loadControlPasswords(exportFile, ingestFile, serviceFile, allowNologin)
	if err != nil {
		return err
	}
	return withStore(func(ctx context.Context, st *store.Store) error {
		changed, err := control.ApplyRoles(ctx, st.Pool(), r, pw)
		if err != nil {
			return err
		}
		state := "unchanged (grants and attributes already as designed)"
		if changed {
			state = "changed"
		}
		fmt.Printf("control roles applied: %s, %s, %s: %s\n", r.Export(), r.Ingest(), r.Service(), state)
		return nil
	})
}

// loadControlPasswords validates the entire requested operation before DB access.
func loadControlPasswords(exportFile, ingestFile, serviceFile string, allowNologin bool) (control.Passwords, error) {
	var pw control.Passwords
	for _, item := range []struct {
		name, path string
		dst        *string
	}{
		{"export", exportFile, &pw.Export}, {"ingest", ingestFile, &pw.Ingest}, {"service", serviceFile, &pw.Service},
	} {
		value, err := readSecret(item.path)
		if err != nil {
			return control.Passwords{}, err
		}
		*item.dst = value
	}
	if !allowNologin {
		for _, item := range []struct{ name, value string }{{"export", pw.Export}, {"ingest", pw.Ingest}, {"service", pw.Service}} {
			if item.value == "" {
				return control.Passwords{}, usageError{fmt.Errorf("--%s-password-file has no effective password: that role would become NOLOGIN (an outage for its transport); pass --allow-nologin to confirm", item.name)}
			}
		}
	}
	return pw, nil
}
