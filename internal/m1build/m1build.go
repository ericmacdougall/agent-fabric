// Package m1build holds the build-time M1 marker (spec M1 comment policy,
// "M1 build"): the release build applies the `m1` Go build tag to fab, fabd
// and fab-live together, and every M1 rule keys off Enabled. The marker row
// in fabric_settings is a database fact; this is a binary fact. An M1 build
// enforces the policy from this constant alone, so a missing marker row can
// stop an M1 build but never turn its policy off.
package m1build

// Name is what `fab policy build` prints for this binary.
func Name() string {
	if Enabled {
		return "m1"
	}
	return "non-m1"
}
