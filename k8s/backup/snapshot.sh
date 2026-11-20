#!/bin/sh
# Run by the postgres-snapshot CronJob. Snapshots $PVC, waits for it to be
# ready, then prunes. If the run fails or is killed before its snapshot is
# ready, it deletes that snapshot, so a broken one never takes a good one's
# place. Pruning keeps the newest $KEEP ready snapshots, and deletes
# not-ready ones older than $STALE_SECONDS, left by a run that died before
# its trap ran. Only snapshots with the managed-by label are touched, so
# manual ones are left alone. kubectl takes the namespace from the pod's
# service account.
set -eu

: "${PVC:?}" "${KEEP:?}" "${WAIT:?}" "${STALE_SECONDS:?}"
label=app.kubernetes.io/managed-by=postgres-snapshot
name="$PVC-$(date -u +%Y%m%d-%H%M%S)"

# Set while this run's snapshot exists but isn't ready yet.
pending=false
cleanup() {
  status=$?
  if [ "$pending" = true ]; then
    echo "Snapshot $name never became ready. Deleting it." >&2
    kubectl delete volumesnapshot "$name" --ignore-not-found --wait=false || true
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 143' TERM INT

pending=true
kubectl create -f - <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: $name
  labels:
    app.kubernetes.io/managed-by: postgres-snapshot
spec:
  source:
    persistentVolumeClaimName: $PVC
EOF

# In the background, because the shell only runs the TERM trap once the
# foreground command returns. "wait" returns as soon as the signal arrives,
# so the trap still runs inside the pod's grace period.
kubectl wait --for=jsonpath='{.status.readyToUse}'=true "volumesnapshot/$name" --timeout="$WAIT" &
wait "$!"
pending=false

# One line per snapshot, oldest first: name, readyToUse, creationTimestamp.
# RFC 3339 UTC timestamps compare correctly as strings.
cutoff=$(date -u -d "@$(( $(date +%s) - STALE_SECONDS ))" +%Y-%m-%dT%H:%M:%SZ)
kubectl get volumesnapshot -l "$label" --sort-by=.metadata.creationTimestamp \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.readyToUse}{"\t"}{.metadata.creationTimestamp}{"\n"}{end}' \
  | awk -F '\t' -v keep="$KEEP" -v cutoff="$cutoff" '
      $2 == "true" { ready[++n] = $1; next }
      $3 < cutoff  { print $1 }
      END { for (i = 1; i <= n - keep; i++) print ready[i] }' \
  | while read -r old; do
      kubectl delete volumesnapshot "$old"
    done
