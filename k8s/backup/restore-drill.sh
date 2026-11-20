#!/usr/bin/env bash
# P5-06 restore drill. Writes a marker row to the live Postgres, snapshots
# its disk, writes a second marker, restores the snapshot to a new PVC and
# starts a temporary Postgres pod on it, built from the StatefulSet's own pod
# template. Passes if the restored database has the first marker and not the
# second. Removes the temporary pod, PVC, snapshot and drill table on exit,
# pass or fail. Run from the repo root with `make restore-drill`. See
# README.md for each step and the expected output.
set -euo pipefail

CONTEXT=${CONTEXT:-aks-v2-platform}
CONTAINER=${CONTAINER:-postgres}
SOURCE_POD=postgres-0
SOURCE_PVC=data-postgres-0
DRILL=postgres-restore # the snapshot, PVC and pod all take this name

k() { kubectl --context "$CONTEXT" -n data "$@"; }

# Runs one SQL statement in a pod over the local socket, as the image's
# superuser. The official image trusts local connections.
sql() {
  # shellcheck disable=SC2016 # expanded by the pod's shell, not this one
  k exec "$1" -c "$CONTAINER" -- sh -c \
    'psql -X -At -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-${POSTGRES_USER:-postgres}}" -c "$1"' \
    sh "$2"
}

cleanup() {
  echo "==> Cleaning up"
  k delete pod "$DRILL" --ignore-not-found --wait=true
  k delete pvc "$DRILL" --ignore-not-found --wait=true
  k delete volumesnapshot "$DRILL" --ignore-not-found --wait=true
  sql "$SOURCE_POD" 'DROP TABLE IF EXISTS restore_drill' >/dev/null || true
}

# Clear anything a killed run left behind, then always clean up on exit.
cleanup
trap cleanup EXIT

run=$(date -u +%Y%m%dT%H%M%SZ)
before="before-$run"
after="after-$run"

echo "==> Writing marker $before to $SOURCE_POD, then CHECKPOINT"
sql "$SOURCE_POD" 'CREATE TABLE IF NOT EXISTS restore_drill (marker text PRIMARY KEY, written_at timestamptz NOT NULL DEFAULT now())'
sql "$SOURCE_POD" "INSERT INTO restore_drill (marker) VALUES ('$before')"
sql "$SOURCE_POD" 'CHECKPOINT'

echo "==> Snapshotting $SOURCE_PVC"
k apply -f - <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: $DRILL
spec:
  source:
    persistentVolumeClaimName: $SOURCE_PVC
EOF
k wait --for=jsonpath='{.status.readyToUse}'=true "volumesnapshot/$DRILL" --timeout=10m

echo "==> Writing marker $after, which the restore must not contain"
sql "$SOURCE_POD" "INSERT INTO restore_drill (marker) VALUES ('$after')"

size=$(k get volumesnapshot "$DRILL" -o jsonpath='{.status.restoreSize}')
class=$(k get pvc "$SOURCE_PVC" -o jsonpath='{.spec.storageClassName}')
echo "==> Restoring to PVC $DRILL ($size, $class)"
k apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $DRILL
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: $class
  resources:
    requests:
      storage: $size
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: $DRILL
EOF

# The StatefulSet's pod template with the restored PVC as its data volume.
# Same image, env, securityContext, resources, on-demand node affinity and
# priority class as postgres-0. A new name label, so neither the postgres
# Service nor its PodDisruptionBudget selects it.
echo "==> Starting pod $DRILL from the postgres StatefulSet's template"
k get statefulset postgres -o json | jq --arg name "$DRILL" '{
  apiVersion: "v1",
  kind: "Pod",
  metadata: {name: $name, labels: {"app.kubernetes.io/name": $name}},
  spec: (.spec.template.spec + {
    volumes: ([(.spec.template.spec.volumes // [])[] | select(.name != "data")]
      + [{name: "data", persistentVolumeClaim: {claimName: $name}}])
  })
}' | k apply -f -
k wait --for=condition=Ready "pod/$DRILL" --timeout=10m

# Ready can come before Postgres accepts connections if the template has no
# readiness probe. Recovery replays the WAL written since the CHECKPOINT.
for _ in $(seq 60); do
  k exec "$DRILL" -c "$CONTAINER" -- pg_isready -q && break
  sleep 5
done
echo "==> Recovery log of $DRILL"
k logs "$DRILL" -c "$CONTAINER" | grep -E 'not properly shut down|redo (starts|done|is not required)|ready to accept connections' || true

echo "==> Markers in the restored database"
restored=$(sql "$DRILL" 'SELECT marker FROM restore_drill ORDER BY written_at')
echo "$restored"

if grep -qx "$before" <<<"$restored" && ! grep -qx "$after" <<<"$restored"; then
  echo "PASS: the restore has $before and not $after"
else
  echo "FAIL: expected $before and not $after" >&2
  exit 1
fi
