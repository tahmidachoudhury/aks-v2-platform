# backup

**TL;DR:** A CronJob in `data` snapshots the Postgres disk every hour and keeps the newest six. `make restore-drill` proves a snapshot restores. It writes a marker row, snapshots the disk, writes a second marker, restores the snapshot to a temporary Postgres pod and checks it has the first marker and not the second. The snapshots live in the node resource group, so the evening destroy deletes them. They are recovery points for the day, not a backup that outlives the cluster. See the ADR [Volume snapshots](../../docs/architecture-decisions.md#volume-snapshots). Redis gets no snapshots. `make backup` applies the CronJob each morning, last in `make cluster`. ArgoCD takes this over in P7.

Filled by P5-06 (#60) and P5-07 (#61). It builds on the `csi-azuredisk-vsc` VolumeSnapshotClass in [storage](../storage/README.md#the-volumesnapshotclass) and the Postgres StatefulSet from P5-01 (#55).

## Resources

| File | Kind | Name | Purpose |
| --- | --- | --- | --- |
| `rbac.yaml` | `ServiceAccount`, `Role`, `RoleBinding` | `postgres-snapshot` | Create, list, watch and delete VolumeSnapshots in `data`. Nothing else |
| `cronjob.yaml` | `CronJob` | `postgres-snapshot` | Runs `snapshot.sh` on the hour |
| `snapshot.sh` | `ConfigMap` (generated) | `postgres-snapshot-<hash>` | Snapshot `data-postgres-0`, wait until it is ready, prune to the newest six |
| `restore-drill.sh` | Script, run locally | | The P5-06 restore drill, run by `make restore-drill` |

## Snapshot consistency

An Azure disk snapshot is crash-consistent. `PGDATA` and the WAL are on the one disk, so Postgres restores by replaying the WAL from the last checkpoint, and every committed transaction is there. Nothing needs `pg_backup_start`, which is for copies that span several volumes or are taken file by file.

- The drill runs `CHECKPOINT` just before the snapshot. This keeps the WAL replay short. Correctness doesn't need it.
- The CronJob doesn't quiesce. A `CHECKPOINT` from the job would need `pods/exec` in `data`, which would let its ServiceAccount run any command in Postgres.

## Restore drill (P5-06)

Needs the Postgres StatefulSet running in `data`, with `postgres-0` ready. Run from the repo root:

```bash
make restore-drill
```

It takes about five minutes. The script, [`restore-drill.sh`](restore-drill.sh), does this:

1. Deletes anything a killed run left behind: the `postgres-restore` pod, PVC and VolumeSnapshot, and the `restore_drill` table.
2. In `postgres-0`, creates the table `restore_drill`, inserts a `before-<time>` marker, then runs `CHECKPOINT`.
3. Creates the VolumeSnapshot `postgres-restore` of `data-postgres-0` and waits for `readyToUse: true`.
4. Inserts an `after-<time>` marker in `postgres-0`. The snapshot was cut before this, so the restore must not contain it.
5. Creates the PVC `postgres-restore` from the snapshot, with the snapshot's `restoreSize` (20Gi) and the source PVC's class (`premium-zrs`).
6. Starts the pod `postgres-restore` from the StatefulSet's own pod template, with the new PVC as its `data` volume. The image, env, securityContext, resources, on-demand node affinity and `data-critical` priority class match `postgres-0`, so the drill can't drift from the StatefulSet. The pod's `app.kubernetes.io/name` label is `postgres-restore`, so neither the `postgres` Service nor its PodDisruptionBudget selects it.
7. Waits for the pod and `pg_isready`, prints Postgres's recovery lines and lists the markers in the restored database.
8. Passes if the `before` marker is there and the `after` marker isn't. Then cleans up on exit, pass or fail: deletes the pod, the PVC (and its disk), the VolumeSnapshot (and the Azure snapshot), and drops `restore_drill` from the live database.

Expected output, with times and WAL positions varying:

```text
==> Cleaning up
==> Writing marker before-20261121T091500Z to postgres-0, then CHECKPOINT
CREATE TABLE
INSERT 0 1
CHECKPOINT
==> Snapshotting data-postgres-0
volumesnapshot.snapshot.storage.k8s.io/postgres-restore created
volumesnapshot.snapshot.storage.k8s.io/postgres-restore condition met
==> Writing marker after-20261121T091500Z, which the restore must not contain
INSERT 0 1
==> Restoring to PVC postgres-restore (20Gi, premium-zrs)
persistentvolumeclaim/postgres-restore created
==> Starting pod postgres-restore from the postgres StatefulSet's template
pod/postgres-restore created
pod/postgres-restore condition met
==> Recovery log of postgres-restore
... LOG:  database system was not properly shut down; automatic recovery in progress
... LOG:  redo starts at 0/1A2B3C4
... LOG:  redo done at 0/1A2B4D8 ...
... LOG:  database system is ready to accept connections
==> Markers in the restored database
before-20261121T091500Z
PASS: the restore has before-20261121T091500Z and not after-20261121T091500Z
==> Cleaning up
pod "postgres-restore" deleted from data namespace
persistentvolumeclaim "postgres-restore" deleted from data namespace
volumesnapshot.snapshot.storage.k8s.io "postgres-restore" deleted from data namespace
```

The "not properly shut down" line is the expected crash recovery. If no WAL was written between the `CHECKPOINT` and the snapshot, Postgres logs `redo is not required` in place of the two `redo` lines.

A failing run prints `FAIL: expected ...`, exits 1 and still cleans up. To inspect a failure before cleanup, follow the steps by hand from the script.

What the drill needs from the cluster:

- Quota: the `data` quota is 750m CPU, 2Gi of memory requests, 6Gi of memory limits, 4 PVCs and 64Gi of storage. It was sized for a second Postgres: Postgres at 250m, Redis at 100m and the drill pod at 250m fit, as do 20Gi, 10Gi and the 20Gi restore. Memory requests are the tight one. Two Postgres requests, Redis's and the CronJob's 64Mi must fit in 2Gi. Past that, the drill pod's creation fails with `exceeded quota`.
- WaitForFirstConsumer: the restored PVC stays `Pending` until the pod is scheduled, and the disk is then created from the snapshot. The script doesn't wait for `Bound`, which would never come.
- Zones: `premium-zrs` gives a ZRS disk, so the restored disk attaches to a node in any zone.
- Snapshot state: restoring before `readyToUse` is `true` fails, so the script waits for it.

## In-place restore

This replaces the live Postgres disk with a snapshot. Warning: it deletes the current disk, and the apps get errors until Postgres is back. The commands work in bash and zsh. Set the snapshot to restore, from `k get volumesnapshot`:

```bash
k() { kubectl --context aks-v2-platform -n data "$@"; }
SNAP=data-postgres-0-20261121-090000
```

Stop the schedule, and take the label off the snapshot being restored, so the CronJob's pruning can't delete it partway through:

```bash
k patch cronjob postgres-snapshot -p '{"spec":{"suspend":true}}'
k label volumesnapshot "$SNAP" app.kubernetes.io/managed-by-
```

Snapshot the current state. Deleting the PVC deletes the disk, and this snapshot is the way back:

```bash
k create job --from=cronjob/postgres-snapshot pre-restore
k wait --for=condition=complete job/pre-restore --timeout=15m
```

Stop Postgres and delete its PVC:

```bash
k scale statefulset postgres --replicas=0
k wait --for=delete pod/postgres-0 --timeout=5m
k delete pvc data-postgres-0
```

Create the PVC under the StatefulSet's name before scaling back up. If the StatefulSet starts first, it creates an empty PVC from its template, and Postgres would initialise a new database:

```bash
k apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-postgres-0
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: premium-zrs
  resources:
    requests:
      storage: 20Gi
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: $SNAP
EOF
k scale statefulset postgres --replicas=1
k wait --for=condition=Ready pod/postgres-0 --timeout=10m
k logs postgres-0 | grep -E 'not properly shut down|redo|ready to accept connections'
```

Expect the same recovery lines as the drill. Then check the data, resume the schedule, and delete the `pre-restore` job. The CronJob may already have removed it, as it keeps one finished job. Delete its snapshot once you no longer need the way back. It has the managed-by label, so the CronJob prunes it in time anyway:

```bash
k exec postgres-0 -- sh -c 'psql -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-${POSTGRES_USER:-postgres}}" -c "\dt"'
k patch cronjob postgres-snapshot -p '{"spec":{"suspend":false}}'
k delete job pre-restore --ignore-not-found
```

The StatefulSet must keep the default `persistentVolumeClaimRetentionPolicy` (`Retain`). With `whenScaled: Delete`, any scale-down deletes the PVC.

## Scheduled snapshots (P5-07)

| Setting | Value | Why |
| --- | --- | --- |
| Schedule | `0 * * * *`, UTC | The cluster lives for about eight hours a day. Hourly gives about eight recovery points, and a restore loses at most an hour of data |
| Retention | Newest 6, by count | Covers the last six hours. Counting can't empty the list if the schedule stops. Snapshots without the label, such as the drill's or one you unlabel, are never pruned |
| Pruning | After the new snapshot is ready | A failed snapshot fails the job before pruning, so a bad run never deletes a good snapshot |
| Concurrency | `Forbid`, one retry, 15 minute deadline | Runs never overlap |
| Image | `alpine/kubectl:1.37.1`, pinned by digest | kubectl plus a shell for the script, about 25 MB. `registry.k8s.io/kubectl` has no shell |
| Pod | Non-root, read-only root filesystem, 10m CPU and 64Mi requested | Meets `restricted` |

Incremental snapshots of a mostly empty 20Gi disk cost pennies: $0.053 per GB a month on the changed blocks, and the evening destroy deletes them.

### Redis

Redis has no scheduled snapshots. It holds only api-gateway's rate-limit counters, which expire within their window, and api-gateway keeps serving without Redis. An empty Redis after a restore loses nothing. To add snapshots later, copy the CronJob with `PVC=data-redis-0` and a label of its own. The AOF restores from a crash-consistent snapshot the same way Postgres does.

### Test the schedule

After `make backup`, check the CronJob:

```bash
k() { kubectl --context aks-v2-platform -n data "$@"; }
k get cronjob postgres-snapshot
```

Expect `SCHEDULE` `0 * * * *` and `SUSPEND` `False`. After the next hour, a snapshot appears:

```bash
k get volumesnapshot -l app.kubernetes.io/managed-by=postgres-snapshot
```

Expect one row named `data-postgres-0-<date>-<time>` with `READYTOUSE` `true`, and one more each hour, up to six.

To test pruning without waiting seven hours, set the retention to 2 and run the job three times by hand. This also prunes the day's hourly snapshots down to two. Each run waits for its snapshot, so allow a couple of minutes each:

```bash
k set env cronjob/postgres-snapshot KEEP=2
for i in 1 2 3; do
  k create job --from=cronjob/postgres-snapshot "prune-test-$i"
  k wait --for=condition=complete "job/prune-test-$i" --timeout=15m
done
k get volumesnapshot -l app.kubernetes.io/managed-by=postgres-snapshot
k logs job/prune-test-3
```

Expect the newest two snapshots listed. The `prune-test-3` log ends with `volumesnapshot.snapshot.storage.k8s.io "<oldest>" deleted from data namespace`. Then restore the CronJob and delete the test jobs. `make backup` sets `KEEP` back to 6 from the manifest:

```bash
make backup
k delete job prune-test-1 prune-test-2 prune-test-3 --ignore-not-found
```

## Validate

```bash
kustomize build k8s/backup | kubeconform -strict -ignore-missing-schemas -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
shellcheck k8s/backup/*.sh
```
