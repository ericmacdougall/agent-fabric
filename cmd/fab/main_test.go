package main

import (
	"os"
	"testing"

	"fabric/internal/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m, "fab")) }
