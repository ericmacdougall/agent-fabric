#!/bin/bash
# goodqa#15805 AC-6: real-reboot drill on a disposable LXC on t2-proxmox.
# Runs as root on t2-proxmox from /root/fabric-drill-15805 with unit.sh,
# fabric-pg.sh and fabd-health.sh next to it. Creates ONE container tagged
# fabric-reboot-drill, proves fabric-pg and a fabd stand-in (the same rendered
# unit) come back after a guest reboot with no manual step, then destroys it.
# The trap destroys only the VMID this run created, after checking its tag.
set -euo pipefail
export LC_ALL=C
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TAG=fabric-reboot-drill
TPL=debian-12-standard_12.12-1_amd64.tar.zst
IP=${DRILL_IP:-10.99.0.240}
LOG=$HERE/drill-$(date -u +%Y%m%dT%H%M%SZ).log
exec > >(tee -a "$LOG") 2>&1
say() { echo "[$(date -u +%T)] $*"; }
START=$(date +%s)
VMID=""
cleanup() {
  local rc=$?
  if [[ -n "$VMID" ]] && pct config "$VMID" >/dev/null 2>&1; then
    if pct config "$VMID" | grep -q "^tags:.*$TAG"; then
      say "cleanup: destroying $VMID (tag $TAG)"
      pct stop "$VMID" >/dev/null 2>&1 || true
      pct destroy "$VMID" --purge >/dev/null 2>&1 && say "cleanup: $VMID destroyed" || say "cleanup: DESTROY FAILED for $VMID"
    else
      say "cleanup: $VMID has no $TAG tag; NOT touching it"
    fi
  fi
  say "drill exit rc=$rc"
}
trap cleanup EXIT
cap() { (( $(date +%s) - START < 1200 )) || { say "STOP: 20 min wall cap"; exit 4; }; }

# the guest IP must be free on vmbr9
if ping -c1 -W1 "$IP" >/dev/null 2>&1; then say "STOP: $IP answers ping; pick another DRILL_IP"; exit 2; fi
[[ -f /var/lib/vz/template/cache/$TPL ]] || { say "downloading $TPL"; pveam download local "$TPL" >/dev/null; }
VMID=$(pvesh get /cluster/nextid)
say "creating LXC $VMID ($TAG) on vmbr9 $IP"
pct create "$VMID" "local:vztmpl/$TPL" --hostname "$TAG" --tags "$TAG" --unprivileged 1 \
  --features nesting=1,keyctl=1 --cores 1 --memory 1024 --swap 0 --rootfs local:8 \
  --net0 "name=eth0,bridge=vmbr9,ip=$IP/24,gw=10.99.0.1" --nameserver 1.1.1.1 --onboot 0 >/dev/null
pct start "$VMID"
for i in $(seq 1 30); do pct exec "$VMID" -- true 2>/dev/null && break; sleep 2; done
g() { pct exec "$VMID" -- bash -c "$1"; }
gu() { pct exec "$VMID" -- runuser -l drill -c "export XDG_RUNTIME_DIR=/run/user/\$(id -u); $1"; }
say "guest: packages"
g 'export DEBIAN_FRONTEND=noninteractive; apt-get -qq update && apt-get -qq install -y docker.io postgresql-client python3 util-linux openssl >/dev/null'
g 'useradd -m -s /bin/bash -G docker drill && loginctl enable-linger drill'
for f in unit.sh fabric-pg.sh fabd-health.sh; do pct push "$VMID" "$HERE/$f" "/home/drill/$f" --perms 0755; done
cap
say "guest: fabric-pg via fabric-pg.sh create (password generated in the guest, never printed)"
g 'install -d -m 0700 -o drill /home/drill/fabd /home/drill/fabd/work; umask 077; openssl rand -hex 24 > /home/drill/pg.pw; chown drill /home/drill/pg.pw; chmod 0644 /home/drill/pg.pw'
gu 'FABRIC_PG_PASSWORD_FILE=/home/drill/pg.pw FABRIC_PG_IMAGE=postgres:16 ~/fabric-pg.sh create'
gu 'docker inspect -f "policy={{.HostConfig.RestartPolicy.Name}} state={{.State.Status}}" fabric-pg'
say "guest: fabd stand-in rendered by render_unit (guard = pg_isready; daemon exits when the DB goes away)"
g 'cat > /home/drill/fab <<"EOF"
#!/bin/bash
ok() { pg_isready -q -h 127.0.0.1 -p 55990; }
case "$1" in
  daemon) ok || exit 1; while ok; do sleep 5; done; exit 1;;
  metrics) ok && echo "fabric_build_info{version=\"drill\"} 1";;
  version) echo drill;;
esac
EOF
chmod 0755 /home/drill/fab
printf "#!/bin/bash\npg_isready -q -h 127.0.0.1 -p 55990\n" > /home/drill/fabd/guard.sh; chmod 0755 /home/drill/fabd/guard.sh
printf "FABRIC_DSN=postgres://drill@127.0.0.1:55990/postgres?application_name=fabd\n" > /home/drill/fabd/env
cat > /home/drill/emit.sh <<"EOF"
#!/bin/bash
printf "%s %s\n" "$(date -u +%FT%TZ)" "$3" >> /home/drill/emits.log
EOF
chmod 0755 /home/drill/emit.sh; chown -R drill /home/drill'
gu 'mkdir -p ~/.config/systemd/user ~/.config/environment.d; echo FABD_HEALTH_EMIT=/home/drill/emit.sh > ~/.config/environment.d/fabd-health.conf
. ~/unit.sh
render_unit live ~/fabd ~/fab /bin/true drill > ~/.config/systemd/user/fabd.service
render_health_service fabd ~/fabd ~/fab ~/fabd-health.sh > ~/.config/systemd/user/fabd-health.service
render_health_timer fabd > ~/.config/systemd/user/fabd-health.timer
systemctl --user daemon-reload
systemd-analyze --user verify fabd.service fabd-health.service fabd-health.timer
systemctl --user enable --now fabd.service fabd-health.timer'
sleep 8
gu 'systemctl --user show fabd -p ActiveState -p Restart -p RestartUSec -p StartLimitIntervalUSec | tr "\n" " "; echo'
gu 'systemctl --user start fabd-health.service; read -r f _ < ~/.local/state/fabd-health/fabd.state; [[ $f == 0 ]] && echo health-pass || echo "health-FAIL state=$f"'
cap
say "pre-reboot: docker stop fabric-pg (a manual stop must not keep it down after a reboot: policy always)"
gu 'docker stop fabric-pg >/dev/null; docker inspect -f "state={{.State.Status}}" fabric-pg'
sleep 15
gu 'echo "fabd while DB is stopped: $(systemctl --user is-active fabd) NRestarts=$(systemctl --user show fabd -p NRestarts --value)"'
say "REBOOT: pct reboot $VMID"
T0=$(date +%s)
pct reboot "$VMID"
for i in $(seq 1 60); do pct exec "$VMID" -- true 2>/dev/null && break; sleep 2; done
say "guest back after $(( $(date +%s) - T0 ))s; waiting up to 180s for fabric-pg and fabd"
ok=0
for i in $(seq 1 36); do
  st=$(gu 'echo "pg=$(docker inspect -f {{.State.Status}} fabric-pg 2>/dev/null) fabd=$(systemctl --user is-active fabd)"' 2>/dev/null || true)
  if [[ "$st" == "pg=running fabd=active" ]]; then
    sleep 20
    st2=$(gu 'echo "pg=$(docker inspect -f {{.State.Status}} fabric-pg) fabd=$(systemctl --user is-active fabd)"')
    [[ "$st2" == "pg=running fabd=active" ]] && { ok=1; break; }
  fi
  sleep 5
done
say "after reboot ($(( $(date +%s) - T0 ))s): $st"
gu 'docker inspect -f "policy={{.HostConfig.RestartPolicy.Name}} state={{.State.Status}} started={{.State.StartedAt}}" fabric-pg; systemctl --user show fabd -p ActiveState -p NRestarts -p ActiveEnterTimestamp | tr "\n" " "; echo; systemctl --user is-active fabd-health.timer; journalctl --user -u fabd -b --no-pager -o cat | grep -c "Start request repeated" || true; cat ~/emits.log 2>/dev/null || echo "emits: none"'
if [[ $ok == 1 ]]; then say "DRILL_RESULT PASS: fabric-pg running and fabd active after a real guest reboot, no manual step"; else say "DRILL_RESULT FAIL"; exit 1; fi
