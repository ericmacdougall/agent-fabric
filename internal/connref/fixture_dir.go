package connref

import "os"

// NewFixtureDirectory creates a new private child of a test-owned temporary
// directory. It never chmods an existing path. The test that owns parent also
// owns removal of this directory after all fixture references have closed.
func NewFixtureDirectory(parent string) (string, error) {
	dir, err := os.MkdirTemp(parent, "fabric-credentials-")
	if err != nil {
		return "", errReference
	}
	return dir, nil
}
