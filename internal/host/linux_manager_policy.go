package host

import (
	"fmt"
	"strings"
)

// workerManagerEscape has no sibling-name or live-name exemptions. Linux runs
// are serialized per worker manager, so only the current unit and exact
// init.scope are outside the escape boundary. Any other unit makes inventory
// fail closed; a reused name can never produce a survivor-free receipt.
func workerManagerEscape(rel, runUnitRel string) bool {
	if rel == runUnitRel || strings.HasPrefix(rel, runUnitRel+"/") {
		return false
	}
	return rel != "init.scope"
}

// workerManagerControlSockets are the stream sockets through which a worker
// could ask a manager to create a process outside its unit. Such a sibling
// would inherit neither the unit's Landlock rules nor its network rules.
// The daemon does not install these restrictions on either manager: they are
// properties of the worker unit only, so the daemon retains its launch path.
func workerManagerControlSockets(uid int) []string {
	runtime := fmt.Sprintf("/run/user/%d", uid)
	return []string{
		runtime + "/bus",
		runtime + "/systemd/private",
		"/run/dbus/system_bus_socket",
		"/run/systemd/private",
	}
}

// workerManagerSockets also covers notification sockets. They are not unit
// creation channels, but making every well-known manager endpoint inaccessible
// avoids exposing a manager facility that a future client could misuse.
func workerManagerSockets(uid int) []string {
	paths := workerManagerControlSockets(uid)
	runtime := fmt.Sprintf("/run/user/%d", uid)
	return append(paths,
		runtime+"/systemd/notify",
		"/run/systemd/notify",
	)
}

// managerInaccessiblePathsProperty is applied to the transient worker unit.
// A leading '-' permits a socket to be absent on a minimal host; existing
// sockets are overmounted as inaccessible by systemd. The property is checked
// by a real transient-unit preflight before any worker launch.
func managerInaccessiblePathsProperty(uid int) string {
	paths := workerManagerSockets(uid)
	for i := range paths {
		paths[i] = "-" + paths[i]
	}
	return "InaccessiblePaths=" + strings.Join(paths, " ")
}

// managerSocketFamilyProperty denies every Unix-domain socket, including
// abstract sockets and buses discovered at nonstandard filesystem paths.
// Fixed InaccessiblePaths entries remain defense in depth for trusted
// preflights, but are not the worker's only manager-access boundary.
const managerSocketFamilyProperty = "RestrictAddressFamilies=~AF_UNIX"

const (
	// PrivatePIDs is the boundary that hides same-UID processes outside the
	// unit. ProtectProc and ProcSubset alone cannot provide that guarantee:
	// the former concerns other UIDs and the latter hides non-PID proc files.
	workerPrivatePIDsProperty = "PrivatePIDs=yes"
	workerProtectProcProperty = "ProtectProc=invisible"
	workerProcSubsetProperty  = "ProcSubset=pid"
)
