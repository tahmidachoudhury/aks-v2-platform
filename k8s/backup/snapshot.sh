#!/bin/sh
# Run by the postgres-snapshot CronJob. Snapshots $PVC, waits for the
# snapshot to be ready, then deletes all but the newest $KEEP scheduled
# snapshots. If the new snapshot never becomes ready the script fails before
# pruning, so a broken run never deletes a good snapshot. Only snapshots with
# the managed-by label are pruned, so manual ones are left alone. kubectl
# takes the namespace from the pod's service account.
set -eu

: "${PVC:?}" "${KEEP:?}"
label=app.kubernetes.io/managed-by=postgres-snapshot
name="$PVC-$(date -u +%Y%m%d-%H%M%S)"

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

kubectl wait --for=jsonpath='{.status.readyToUse}'=true "volumesnapshot/$name" --timeout=10m

names=$(kubectl get volumesnapshot -l "$label" --sort-by=.metadata.creationTimestamp -o name)
excess=$(( $(printf '%s\n' "$names" | wc -l) - KEEP ))
if [ "$excess" -gt 0 ]; then
  printf '%s\n' "$names" | head -n "$excess" | xargs kubectl delete
fi
