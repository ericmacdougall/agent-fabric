# shellcheck shell=bash
# Unit renderers for the fabd installer (goodqa#15587, goodqa#15805). Sourced by
# install.sh and by tools/fabd-install/install_test.go; prints unit files on stdout.

# render_unit MODE SVC FAB FAKE HOSTLABEL
#   The fabd daemon unit. After a host reboot the database container can come up
#   after fabd's first start, so the unit keeps retrying (no start limit, a
#   10-second pace) instead of giving up after five tries; that give-up left
#   live down for 15.5 hours on 2026-11-12 (goodqa#15805). A manual
#   `systemctl --user stop` still stops it: Restart= never fights a stop.
render_unit() {
  local mode=$1 svc=$2 fab=$3 fake=$4 host=$5
  cat <<UNITF
[Unit]
Description=Fabric fabd ($mode, no dispatch, recorded fake provider only; goodqa#15587)
StartLimitIntervalSec=0

[Service]
EnvironmentFile=$svc/env
ExecStartPre=$svc/guard.sh
ExecStart=/usr/bin/flock -n $svc/fabd.lock $fab daemon --no-dispatch --provider $fake --trust-provider --work-root $svc/work --tier-table $svc/tiers.json --rung R0 --host $host
Restart=always
RestartSec=10
KillSignal=SIGTERM
TimeoutStopSec=15

[Install]
WantedBy=default.target
UNITF
}

# render_health_service UNIT SVC FAB HEALTH_SCRIPT
#   One health check of UNIT (fabd-health.sh). It reads fabd's own environment
#   file so `fab metrics` reaches the same database; the script never prints it.
render_health_service() {
  local unit=$1 svc=$2 fab=$3 health=$4
  cat <<UNITF
[Unit]
Description=Fabric $unit health check: critical ops-watch alert when $unit stays down (goodqa#15805)

[Service]
Type=oneshot
EnvironmentFile=$svc/env
ExecStart=$health $unit $fab
TimeoutStartSec=60
UNITF
}

# render_health_timer UNIT
render_health_timer() {
  local unit=$1
  cat <<UNITF
[Unit]
Description=Run the $unit health check every 2 minutes (goodqa#15805)

[Timer]
OnBootSec=3min
OnUnitActiveSec=2min
Unit=$unit-health.service

[Install]
WantedBy=timers.target
UNITF
}
