//go:build !linux

package main

import (
	"context"
	"io"
)

func runOnPty(context.Context, string, []string, []string, io.Reader, func([]byte)) error {
	return errNoPty
}
