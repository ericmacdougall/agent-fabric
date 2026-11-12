#!/bin/bash
# The fabric-pg Postgres container on t2-dev (goodqa#15805).
#
#   fabric-pg.sh check    exit 0 only when the container restarts on boot
#                         (RestartPolicy unless-stopped or always)
#   fabric-pg.sh ensure   set RestartPolicy always on the existing
#                         container; never recreates it or touches its volume.
#                         always, not unless-stopped: a `docker stop` before a
#                         reboot (a drill, maintenance) must not keep it down
#                         after the reboot.
#   fabric-pg.sh create   create the container once, with --restart always;
#                         refuses if it exists. The superuser password is read by
#                         Postgres from FABRIC_PG_PASSWORD_FILE, mounted read-only;
#                         its value never reaches argv, the environment or a log.
#
# The container was first made by hand on 2026-11-06 with no restart policy, so
# after the 2026-11-12 03:14Z reboot it stayed down and fabd gave up.
set -euo pipefail
NAME=${FABRIC_PG_NAME:-fabric-pg}
PORT=${FABRIC_PG_PORT:-55990}
VOLUME=${FABRIC_PG_VOLUME:-fabric-pg-data}
IMAGE=${FABRIC_PG_IMAGE:-postgres:16}
for v in NAME PORT VOLUME IMAGE; do
  [[ "${!v}" =~ ^[A-Za-z0-9._:/@-]+$ ]] || { echo "fabric-pg: $v has unsafe characters" >&2; exit 2; }
done

policy() {
  docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$NAME" 2>/dev/null
}

case "${1:-}" in
  check)
    p=$(policy) || { echo "fabric-pg: no container named $NAME" >&2; exit 1; }
    case "$p" in
      unless-stopped|always) echo "fabric-pg: $NAME restart policy $p"; exit 0;;
      *) echo "fabric-pg: $NAME restart policy is '$p'; it will not come back after a reboot (run: fabric-pg.sh ensure)" >&2; exit 1;;
    esac;;
  ensure)
    policy >/dev/null || { echo "fabric-pg: no container named $NAME; ensure never creates one (see: fabric-pg.sh create)" >&2; exit 1; }
    docker update --restart always "$NAME" >/dev/null
    exec "$0" check;;
  create)
    if policy >/dev/null; then
      echo "fabric-pg: container $NAME already exists; create refuses (use ensure)" >&2; exit 1
    fi
    pw=${FABRIC_PG_PASSWORD_FILE:-}
    [[ -n "$pw" && -r "$pw" && -s "$pw" ]] || { echo "fabric-pg: FABRIC_PG_PASSWORD_FILE must name a readable, non-empty file" >&2; exit 2; }
    [[ "$pw" =~ ^/[A-Za-z0-9/._-]+$ ]] || { echo "fabric-pg: FABRIC_PG_PASSWORD_FILE must be an absolute path of [A-Za-z0-9/._-] (a relative one would become a docker volume name); the postgres uid in the container must be able to read it" >&2; exit 2; }
    docker run -d --name "$NAME" --restart always \
      -p "127.0.0.1:$PORT:5432" \
      -v "$VOLUME:/var/lib/postgresql/data" \
      -v "$pw:/run/secrets/fabric-pg.pw:ro" \
      -e POSTGRES_PASSWORD_FILE=/run/secrets/fabric-pg.pw \
      "$IMAGE" >/dev/null
    # a container that cannot start (say, the postgres uid cannot read the
    # password file) crash-loops under --restart always; report it, not the
    # policy alone (PR 51 receipt F5)
    ready=0
    for _ in $(seq 1 ${FABRIC_PG_READY_TRIES:-30}); do
      st=$(docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$NAME" 2>/dev/null || true)
      if [[ "$st" == "true 0" ]] && docker exec "$NAME" pg_isready -q >/dev/null 2>&1; then ready=1; break; fi
      sleep 2
    done
    (( ready )) || { echo "fabric-pg: $NAME did not become ready (state: ${st:-unknown}); see docker logs $NAME" >&2; exit 1; }
    exec "$0" check;;
  *)
    echo "usage: fabric-pg.sh check|ensure|create" >&2; exit 2;;
esac
