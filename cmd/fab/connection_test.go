package main

import (
	"os"
	"testing"
)

func unsetConnectionEnv(t *testing.T, name string) {
	t.Helper()
	value, present := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal("clear synthetic environment")
	}
	t.Cleanup(func() {
		if present {
			os.Setenv(name, value)
		} else {
			os.Unsetenv(name)
		}
	})
}
