// Package hostid is the one default host id, shared by fabd and the native
// runner (goodqa#15087): an attempt records the host it runs on, and the
// reconcile pass for a host acts on that host's attempts only, so both
// launchers must name a host the same way.
package hostid

import (
	"os"
	"strings"

	"fabric/internal/domain"
)

// Default is "h_" plus this machine's short host name, lower-cased, with
// anything outside [a-z0-9] mapped to '-'.
func Default() domain.HostID { return domain.HostID("h_" + Name()) }

// Name is the cleaned short host name.
func Name() string {
	h, _ := os.Hostname()
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(h))
}
