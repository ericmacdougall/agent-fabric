#!/bin/sh
# Real Linux containment controls. This script starts and stops transient
# systemd user units and signals only the processes those units create.
# It must pass on the target Linux host before containment is advertised.
set -eu

if [ "$(uname -s)" != "Linux" ]; then
    echo "linux_runtime_controls.sh must run on Linux" >&2
    exit 2
fi
if [ -z "${FABRIC_WORKER_USER:-}" ]; then
    echo "FABRIC_WORKER_USER must name the dedicated worker account" >&2
    exit 2
fi
if ! command -v setfacl >/dev/null 2>&1; then
    echo "setfacl is required for isolated worker grant-leaf permissions" >&2
    exit 2
fi

# goodqa#15299: build and test with the pinned toolchain only. FAB_GO names
# the go binary (default: the go on PATH); GOTOOLCHAIN=local stops a
# go.mod toolchain line from downloading a different Go.
GO=${FAB_GO:-go}
export GOTOOLCHAIN=local

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$root"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/fab-linux-runtime.XXXXXX")
bindir=$(mktemp -d "${TMPDIR:-/tmp}/fab-linux-runtime-bin.XXXXXX")
workerroot=$(mktemp -d "${TMPDIR:-/tmp}/fab-linux-worker.XXXXXX")
trap 'rm -rf "$scratch" "$bindir" "$workerroot"' EXIT HUP INT TERM

# Only the executable directory is generally traversable. The worker root is
# controller-owned 0700 with execute-only worker access; inherited ACLs grant
# rwx only below generated grant leaves, never at the root itself.
chmod 0755 "$bindir"
chmod 0700 "$workerroot"
setfacl -m "u:${FABRIC_WORKER_USER}:--x" -m "d:u:${FABRIC_WORKER_USER}:rwx" "$workerroot"
"$GO" build -o "$bindir/fab" ./cmd/fab
chmod 0755 "$bindir/fab"
export FABRIC_FAB_BIN="$bindir/fab"
export FABRIC_WORKER_ROOT="$workerroot"
export FABRIC_HOST_CONTROLS=1
export FABRIC_EXPECT_CONTAINMENT=1

# Native controls cover delayed submission and blocked-manager deadlines,
# launcher reaping, fast commands, same-UID ptrace/memory/FD/signal attacks,
# abstract buses, UDP/DNS, lock replacement, dedicated-worker traversal,
# mediator cleanup, concurrent Stop and verified failover.
"$GO" test ./internal/host \
    -run '^(TestNegativeControls|TestLinuxManagerNegativeControls|TestLinuxRuntimeSerializationCleanupFailover|TestLinuxRuntimeDelayedManagerReadiness|TestLinuxRuntimeDelayedSubmissionCannotEscapeCleanup|TestLinuxRuntimeBlockedManagerReturnsRetryableHandle|TestLinuxRuntimeFastSuccessfulCommand|TestLinuxRuntimeSameUIDProcessAttacksDenied|TestLinuxRuntimeDedicatedWorkerTraversal|TestLinuxRuntimeWorkerCannotReplaceManagerLock)$' \
    -count=1 -v
