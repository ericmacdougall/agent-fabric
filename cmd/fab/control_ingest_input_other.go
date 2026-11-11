//go:build !unix

package main

import (
	"context"
	"errors"
	"io"
	"os"
)

func newIngestInput(context.Context, *os.File) (io.ReadCloser, error) {
	return nil, errors.New("ingest requires cancellable Unix stdin")
}
