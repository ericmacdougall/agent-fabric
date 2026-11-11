//go:build !linux

package connref

func readProtected(string) ([]byte, error)    { return nil, ErrUnsupported }
func parseMaterial(string) (*material, error) { return nil, ErrUnsupported }
func makeRef(*material, string) (*Ref, error) { return nil, ErrUnsupported }

func parsePasswordMaterial(string, string) (*material, error) { return nil, ErrUnsupported }
