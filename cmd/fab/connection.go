package main

import (
	"errors"

	"fabric/internal/connref"
)

type connectionRef = connref.Ref

func connectionDSN(name string, purpose connref.Purpose, required bool) (string, *connref.Ref, error) {
	r, err := connref.ResolveEnv(name, purpose)
	if err != nil {
		return "", nil, err
	}
	dsn, err := r.DSN()
	if err != nil || (required && dsn == "") {
		r.Close()
		if err != nil {
			return "", nil, err
		}
		return "", nil, errors.New(name + " is not set")
	}
	return dsn, r, nil
}

func appConnection() (string, *connref.Ref, error) {
	return connectionDSN("FABRIC_DSN", connref.App, true)
}

func runnerConnection(name string, required bool) (string, *connref.Ref, error) {
	return connectionDSN(name, connref.Runner, required)
}
