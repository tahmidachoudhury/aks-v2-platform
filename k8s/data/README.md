# data

**TL;DR:** Postgres 16 and Redis 7.4 run in the `data` namespace, each as a single-replica StatefulSet on a Premium ZRS disk (20Gi Postgres, 10Gi Redis). Redis writes an append-only file. Both passwords come from Key Vault through ESO. Both pods run as UID 999 under the `restricted` Pod Security Standard. Node affinity keeps them on on-demand NAP nodes. A PodDisruptionBudget blocks voluntary eviction. The `data-critical` priority class lets them preempt app pods when `general` is full. The data does not survive the nightly platform destroy, since the disks live in the node resource group. This is deliberate: the sandbox starts each morning with an empty database that migrations rebuild. `make data` applies the folder after `external-dns`. ArgoCD takes it over in P7.

Filled by P5-01 (#55), P5-02 (#56) and P5-03 (#57). The decision to run Postgres in the cluster is in [Postgres](../../docs/architecture-decisions.md#postgres).

## Resources

| Path | Kind | Name | Purpose |
| --- | --- | --- | --- |
| `priorityclass.yaml` | `PriorityClass` | `data-critical` | Lets Postgres and Redis preempt app pods |
| `postgres/statefulset.yaml` | `StatefulSet` | `postgres` | One replica, volumeClaimTemplate `data`, so the PVC is `data-postgres-0` |
| `postgres/service.yaml` | `Service` (headless) | `postgres` | `postgres.data.svc.cluster.local:5432` |
| `postgres/external-secret.yaml` | `ExternalSecret` | `postgres-credentials` | Key Vault `postgres-password` into Secret `postgres-credentials`, key `password` |
| `postgres/pdb.yaml` | `PodDisruptionBudget` | `postgres` | `maxUnavailable: 0` |
| `postgres/init-app-role.sh` | `ConfigMap` `postgres-init` | | Creates the `app` role on first start |
| `redis/statefulset.yaml` | `StatefulSet` | `redis` | One replica, volumeClaimTemplate `data`, so the PVC is `data-redis-0` |
| `redis/service.yaml` | `Service` (headless) | `redis` | `redis.data.svc.cluster.local:6379` |
| `redis/external-secret.yaml` | `ExternalSecret` | `redis-credentials` | Key Vault `redis-password` into Secret `redis-credentials`, key `password` |
| `redis/pdb.yaml` | `PodDisruptionBudget` | `redis` | `maxUnavailable: 0` |

### Why `k8s/data/<component>`

There is one folder per namespace and one subfolder per component. Postgres and Redis share the namespace, the priority class and the morning step, so `kubectl apply -k k8s/data` applies both. Each subfolder is its own kustomization, so `kubectl apply -k k8s/data/postgres` works alone. We own these manifests, so they are Kustomize, not a chart ([Manifest tooling](../../docs/architecture-decisions.md#manifest-tooling)). The restore drill's second Postgres stays out of this tree, so `make data` never creates it.

## Connecting

| | Postgres | Redis |
| --- | --- | --- |
| Host | `postgres.data.svc.cluster.local` | `redis.data.svc.cluster.local` |
| Port | 5432 | 6379 |
| Database | `orders` | n/a |
| App user | `app`, owner of `orders` | none, `requirepass` only |
| Key Vault secret | `postgres-password` | `redis-password` |
| Secret in `data` | `postgres-credentials`, key `password` | `redis-credentials`, key `password` |
| URL | `postgres://app:<password>@postgres.data.svc.cluster.local:5432/orders?sslmode=disable` | `redis://:<password>@redis.data.svc.cluster.local:6379` |

The services and the migration Job run in `apps` and can't read Secrets in `data`. They build their own `DATABASE_URL` or `REDIS_URL` from the same Key Vault secret, with an ExternalSecret template in `apps` (P6-06). The URL is the [compose](../../app/docker-compose.yml) one with the host and password swapped. The server has no TLS, so `sslmode=disable` stays.

Postgres users:

- `postgres`, the superuser. Use it from inside the pod, where local connections need no password: `kubectl -n data exec postgres-0 -- psql -U postgres -d orders`.
- `app`, the role all seven services use. It owns `orders`, so it can create tables, which the migration Job needs. `init-app-role.sh` creates it on the first start against an empty disk.

Both use the `postgres-password` value, since the vault holds one Postgres secret ([keyvault](../../infra/modules/keyvault/README.md)). Remote connections need the password (`scram-sha-256`). Only connections from inside the pod are trusted.

## Pod Security

`data` enforces `restricted` ([namespaces](../namespaces/README.md#pod-security)). Both pods:

- Run as `runAsUser: 999`, `runAsGroup: 999`, `fsGroup: 999`, with `runAsNonRoot: true`. 999 is the `postgres` and `redis` user in the official Debian images. The Alpine images use other IDs, so the tags stay on `bookworm`.
- Use the `RuntimeDefault` seccomp profile, drop all capabilities and set `allowPrivilegeEscalation: false`.
- Have a read-only root filesystem. Postgres writes only to its disk and to `emptyDir` volumes at `/var/run/postgresql` and `/tmp`. Redis writes only to `/data`.
- Mount no ServiceAccount token and set `enableServiceLinks: false`, so Kubernetes doesn't inject `POSTGRES_PORT` or `REDIS_PORT` variables that the images could misread.

Postgres's disk is mounted at `/var/lib/postgresql/data` and `PGDATA` is `/var/lib/postgresql/data/pgdata`, because the disk root holds `lost+found` and `initdb` needs an empty directory. The Redis password reaches `redis-server` on stdin and the Postgres password goes through the environment, so neither shows in a process list.

## Images

| Component | Image | Why this version |
| --- | --- | --- |
| Postgres | `postgres:16.16-bookworm`, pinned by digest | Major 16 matches local development in `app/docker-compose.yml`. Supported until November 2028 |
| Redis | `redis:7.4.12-bookworm`, pinned by digest | Major 7 matches compose |

Both use the multi-arch index digest, so the tag can't move. To upgrade, change the tag and digest together. A Postgres major upgrade needs `pg_upgrade` or a dump and restore, which the nightly rebuild makes moot here.

## Probes and shutdown

| | Startup | Readiness | Liveness |
| --- | --- | --- | --- |
| Postgres | `pg_isready` on 127.0.0.1, every 5s, up to 5 minutes | `pg_isready`, every 10s | `pg_isready`, 6 misses at 10s |
| Redis | none | `redis-cli ping` answers `PONG`, every 10s | TCP 6379, 6 misses at 10s |

- `pg_isready` uses TCP, not the socket. On first start the image's temporary init server listens only on the socket, so the pod turns Ready only when the real server is up. It needs no password.
- Redis answers `LOADING` while it replays the AOF, so it isn't Ready until the replay ends. The port is open during replay, so liveness passes.
- `redis-cli` reads the password from `REDISCLI_AUTH`, which the pod sets, so probes and `kubectl exec` sessions need no `-a`.
- Kubernetes stops a pod with SIGTERM. Postgres treats that as a smart shutdown, which waits for every client to disconnect, and pooled connections never do. A `preStop` hook runs `pg_ctl stop -m fast` first. That rolls back open transactions and writes a checkpoint, so the next start needs no crash recovery. Grace period: 60 seconds.
- Redis fsyncs the AOF and exits on SIGTERM. `exec` keeps `redis-server` as PID 1 so it gets the signal.

## Resources and quota

| Container | CPU request | Memory request | Memory limit |
| --- | --- | --- | --- |
| Postgres | 250m | 512Mi | 1Gi |
| Redis | 50m | 64Mi | 256Mi |
| Restore drill's second Postgres, if it copies these | 250m | 512Mi | 1Gi |
| **Total with the drill** | **550m of 750m** | **1,088Mi of 2Gi** | **2,304Mi of 6Gi** |

PVCs: three of four with the drill. Storage: 20 + 10 + 20 = 50Gi of 64Gi. These are inside the [data quota](../namespaces/README.md#sizes). There is no CPU limit, as the namespace README explains. Postgres's default `shared_buffers` (128MB) fits the 1Gi limit.

## Scheduling: off spot, and safe to move

### Spot

Both StatefulSets require `karpenter.sh/capacity-type In [on-demand]`. NAP labels every node it creates with its capacity type:

- `general` nodes carry `on-demand` and qualify.
- `spot` nodes carry `spot` and the `kubernetes.azure.com/scalesetpriority=spot:NoSchedule` taint.
- System nodes carry no capacity-type label and have the `CriticalAddonsOnly` taint.

Neither pod tolerates the spot taint, so the taint already keeps them off spot. The affinity still holds if someone adds a toleration by mistake.

### Disruption

Each component has a PodDisruptionBudget with `maxUnavailable: 0`. It refuses every eviction: `kubectl drain`, NAP consolidation and node expiry. NAP leaves the node alone, even when it is underused. Two things still go through:

- `kubectl delete pod`, which isn't an eviction. The acceptance tests use it.
- Evicting a pod that isn't Ready (`unhealthyPodEvictionPolicy: AlwaysAllow`), so a broken Postgres can't block a drain forever.

To drain a node on purpose, delete the pod, then drain the node. The cost is that NAP can't repack the node Postgres or Redis sits on. With one or two `general` nodes that loss is small, and cheaper than an unplanned restart of the only database.

### Priority

`data-critical` is 1,000,000 and `preemptionPolicy` is `PreemptLowerPriority`. App pods have the default priority, 0. `general` is capped at 4 vCPUs. If apps fill it when Postgres needs a node, for example after a zone move, the scheduler evicts app pods, so Postgres doesn't wait in `Pending`. The class is far below `system-cluster-critical` (2,000,000,000), so it never preempts AKS add-ons.

### Spread

There is one replica of each, so a zone spread constraint has nothing to spread. Postgres and Redis may share a node. Separating them gains nothing, since the apps need both, and could force a second node. The disks protect against zone loss instead.

## Zones: ZRS against LRS

`premium-zrs` creates `Premium_ZRS` disks ([storage](../storage/README.md)). When the Postgres pod moves to a node in another zone:

| | ZRS (what we use) | LRS |
| --- | --- | --- |
| Where the disk lives | Copied synchronously across the three zones | One zone |
| PV node affinity | No single-zone term | `topology.disk.csi.azure.com/zone In [uksouth-N]` |
| The pod moves to another zone | The disk detaches and attaches to the new node. The pod starts with its data after about a minute | The scheduler places the pod only in the disk's zone. NAP creates a node there if none has room. If the zone is down, or NAP can't get the size there (zone 1 lacks `D2s_v6` and `D4s_v6`), the pod stays `Pending` |
| A zone outage | The disk is still readable from the other two zones | The data is out of reach until the zone returns |
| Cost, 20Gi billed as 32 GiB | About $9.58 a month | About $6.39 a month |

ZRS protects the disk, not the service. One replica still means downtime while the pod moves: about a minute of detach and attach, plus a new node if NAP has to make one. The [Postgres ADR](../../docs/architecture-decisions.md#postgres) has the sources. To see it on the cluster, use the zone drill in the acceptance tests below.

## The nightly destroy

`make down` destroys the platform root every evening, including the cluster and its node resource group, `rg-aks-v2-platform-nodes`. CSI-created PVC disks and the snapshots live in that group ([storage](../storage/README.md#the-volumesnapshotclass)). Postgres and Redis start every morning on empty disks.

Decision: accept it. The sandbox starts each morning with an empty database.

- The brief's durability tests run within a day on one cluster: pod restart, reschedule, zone move, snapshot and restore.
- The [Postgres ADR](../../docs/architecture-decisions.md#postgres) already treats the data as disposable. The migration Job (P5-05, #59) creates the schema on each deploy.
- The first start of the day re-reads `postgres-password` from Key Vault, so an overnight rotation needs no `ALTER ROLE`.
- Keeping the data needs a managed disk in the persistent root, a role for the cluster identity on it, and a static PV bound to `data-postgres-0` through a `claimRef`. The disk would cost money while the cluster is down. The init script and the vault password would also disagree after any rotation.

Postgres is the only state that must survive a pod, node or zone failure. Nothing here needs to outlive the cluster. To keep data, for example a long-running demo dataset, use a static PV:

1. Create an `azurerm_managed_disk`, `Premium_ZRS`, 20 GiB, in the persistent resource group, with `prevent_destroy`.
2. Give the cluster's control plane identity a role assignment on that disk. The disk CSI driver attaches with that identity, and it has rights only on the node resource group today.
3. Apply a `PersistentVolume` before the StatefulSet, with `csi.volumeHandle` set to the disk's ID, `persistentVolumeReclaimPolicy: Retain`, and a `claimRef` to `data/data-postgres-0`. Keep the disk ID in a gitignored env file, as the vault URL is.

## Morning apply

`make cluster` runs `make data` last, after `external-dns`. It needs:

- `make namespaces`, for `data`, its quota and Pod Security.
- `make storage`, for `premium-zrs`.
- `make external-secrets`, for the ClusterSecretStore. It already allows `data`.
- `make node-pools`, so there is an on-demand node.

From the repo root:

```bash
make data
```

It applies `k8s/data`, waits up to 2 minutes for both ExternalSecrets to sync, then waits up to 10 minutes for each StatefulSet. A first start takes a few minutes: NAP may create a node, the disk must be created and attached, and Postgres runs `initdb`.

## Acceptance test

These need a live cluster. Run them from the repo root after `make data`. Each test cleans up its own data. They use `kubectl delete pod`, which the PodDisruptionBudget doesn't block.

The StatefulSet recreates a deleted pod with the same name. This helper waits until that pod is Ready:

```bash
wait_ready() { until kubectl -n data wait --for=condition=Ready "pod/$1" --timeout=5m 2>/dev/null; do sleep 2; done; }
```

### P5-01 (#55): Postgres is Ready and its data survives a pod delete

```bash
kubectl -n data get statefulset,pod,pvc,externalsecret
kubectl -n data exec postgres-0 -- psql -U postgres -d orders -c "CREATE TABLE p5_test (x int); INSERT INTO p5_test VALUES (42);"
kubectl -n data delete pod postgres-0
wait_ready postgres-0
kubectl -n data exec postgres-0 -- psql -U postgres -d orders -Atc "SELECT x FROM p5_test"
kubectl -n data exec postgres-0 -- psql -U postgres -d orders -c "DROP TABLE p5_test"
```

Expect `postgres-0` `1/1 Running`, `data-postgres-0` `Bound` at 20Gi on `premium-zrs`, both ExternalSecrets `SecretSynced`, then `42` after the delete.

The `app` role logs in over the network with the vault password. This is a password login through the Service name, not a local trusted one. The password is never printed:

```bash
kubectl -n data exec postgres-0 -- sh -c \
  'PGPASSWORD="$POSTGRES_PASSWORD" psql -h postgres.data.svc.cluster.local -U app -d orders -Atc "SELECT current_user"'
```

Expect `app`.

### P5-02 (#56): Redis keys survive a pod delete and the AOF is on the volume

```bash
kubectl -n data exec redis-0 -- redis-cli SET p5-test survives
kubectl -n data delete pod redis-0
wait_ready redis-0
kubectl -n data exec redis-0 -- redis-cli GET p5-test
kubectl -n data exec redis-0 -- redis-cli CONFIG GET appendonly
kubectl -n data exec redis-0 -- ls -l /data/appendonlydir
kubectl -n data exec redis-0 -- redis-cli DEL p5-test
```

Expect `survives`, then `appendonly yes`, then `appendonly.aof.1.base.rdb`, `appendonly.aof.1.incr.aof` and `appendonly.aof.manifest`. With AOF on, Redis loads only the AOF at startup, never `dump.rdb`, so a returning key proves the AOF held it.

### P5-03 (#57): never on spot, eviction blocked, zone behaviour

Show each pod's node, capacity type and zone:

```bash
for p in postgres-0 redis-0; do
  kubectl get node "$(kubectl -n data get pod "$p" -o jsonpath='{.spec.nodeName}')" \
    -L karpenter.sh/capacity-type,karpenter.sh/nodepool,topology.kubernetes.io/zone --no-headers
done
kubectl -n data get pods -o custom-columns=NAME:.metadata.name,PRIORITY:.spec.priorityClassName,VALUE:.spec.priority
kubectl -n data get pdb
```

Expect `on-demand` and `general` for both pods, `data-critical` and `1000000` for both, and `ALLOWED DISRUPTIONS 0` on both budgets.

To confirm the affinity rules out every spot and system node, list the nodes that match the rule. This changes nothing:

```bash
kubectl get nodes -l karpenter.sh/capacity-type=on-demand -L karpenter.sh/nodepool
```

Only `general` nodes should appear.

A drain is refused. This server-side dry run cordons and evicts nothing:

```bash
NODE=$(kubectl -n data get pod postgres-0 -o jsonpath='{.spec.nodeName}')
kubectl drain "$NODE" --ignore-daemonsets --delete-emptydir-data --dry-run=server --timeout=20s
```

Expect `Cannot evict pod as it would violate the pod's disruption budget` for each `data` pod on that node.

The zone drill moves Postgres off its node and records the new zone and the disk behaviour. It costs a minute or two of Postgres downtime:

```bash
PV=$(kubectl -n data get pvc data-postgres-0 -o jsonpath='{.spec.volumeName}')
kubectl get pv "$PV" -o jsonpath='{.spec.csi.volumeAttributes.skuName}{"\n"}{.spec.nodeAffinity}{"\n"}'
NODE=$(kubectl -n data get pod postgres-0 -o jsonpath='{.spec.nodeName}')
kubectl get node "$NODE" -L topology.kubernetes.io/zone --no-headers
kubectl cordon "$NODE"
kubectl -n data delete pod postgres-0
wait_ready postgres-0
kubectl get node "$(kubectl -n data get pod postgres-0 -o jsonpath='{.spec.nodeName}')" -L topology.kubernetes.io/zone --no-headers
kubectl -n data get events --field-selector involvedObject.name=postgres-0 | grep -iE 'attach|schedul'
kubectl uncordon "$NODE"
```

Expect `Premium_ZRS` and node affinity with no single-zone term. The pod lands on another node, possibly a new NAP node, with `SuccessfulAttachVolume`. NAP picks the zone by price and capacity, so the new node can be in the same zone. Repeat until the zone changes. Record the zone before and after, and the time from delete to Ready. Write the results under [Zones](#zones-zrs-against-lrs).

## Validate

```bash
kustomize build k8s/data | kubeconform -strict -ignore-missing-schemas -summary
```

kubeconform has no schema for ExternalSecret, so it skips the two. To check them against the CRD, extract the `v1` schema from `kubectl get crd externalsecrets.external-secrets.io -o json` and pass it with `-schema-location`, as [external-secrets](../external-secrets/README.md#validate) describes.
