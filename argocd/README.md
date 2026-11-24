# argocd

**TL;DR:** Argo CD deploys everything in the cluster from this repo, as an app of apps. `make up` applies [`infra/argocd`](../infra/argocd/README.md), which installs Argo CD and one root Application. The root renders the Helm chart in [`apps/`](apps), which holds 22 child Applications: one per platform component, one for data, one each for backup and migrations, and one per service. Sync waves bring them up in order, and the root waits for each wave to turn Healthy before the next. Every Application syncs automatically with prune and selfHeal, except that data never prunes. OpenTofu passes values that must stay out of this public repo, such as client IDs, as Helm values on the root. The UI is reached with `kubectl port-forward` only. To roll back one service, revert its tag bump in Git. The runbook is in [Rollback](#rollback).

Filled by P7-01 (#73) to P7-06 (#78).

## Layout

```text
argocd/apps/
  Chart.yaml
  values.yaml           repo, revision, and which services use a workload identity
  values.schema.json    refuses a render without every value from OpenTofu, or with a malformed one
  values.example.yaml   example values, for helm template and kubeconform only
  templates/
    _helpers.tpl        the head and tail every child shares: wave, destination, sync policy
    platform.yaml       node-pools to keda
    data.yaml           data, backup, migrations
    services.yaml       the nine services
```

Chart versions are pinned in `platform.yaml` and nowhere else. Each chart's values stay in `k8s/<component>/values.yaml`. The Application reads them through a second source with `ref: values`.

## How the cluster comes up

1. `make up` applies platform, fetches a kubeconfig, then applies `infra/argocd`.
2. `infra/argocd` creates the `argocd` namespace, installs the argo-cd chart and creates the root Application with the values from OpenTofu ([infra/argocd](../infra/argocd/README.md)).
3. The root syncs `argocd/apps` and creates the children wave by wave. Each child syncs itself, and the root moves on only when the whole wave is Healthy.

| Wave | Applications | Source | Why here |
| --- | --- | --- | --- |
| 0 | node-pools | `k8s/node-pools` | Nothing outside the system pool schedules without a NodePool. Argo CD tolerates `CriticalAddonsOnly`, so it runs before this |
| 1 | namespaces, storage | `k8s/namespaces`, `k8s/storage` | The charts don't create `platform`. data needs `premium-zrs` |
| 2 | external-secrets | chart 2.15.0 | Its CRDs and webhook come before any store or ExternalSecret |
| 3 | external-secrets-store | `k8s/external-secrets/store` | The ClusterSecretStore every ExternalSecret reads |
| 4 | traefik | chart 42.4.1 | The ingress class the issuers' HTTP-01 solver uses |
| 5 | cert-manager, external-dns, reloader, keda | charts v1.21.3, 1.23.0, 2.2.20, 2.21.1, plus `k8s/cert-manager` | cert-manager's ClusterIssuers sit one wave after its chart inside that Application. KEDA's CRDs come before the worker's ScaledObject |
| 6 | data | `k8s/data` | Postgres and Redis. Their ExternalSecrets need the store |
| 7 | backup | `k8s/backup` | The snapshot CronJob, once Postgres exists |
| 8 | migrations | `k8s/migrations` | The schema, before any service starts ([migrations](../k8s/migrations/README.md)) |
| 9 | the nine services | `k8s/apps/<service>/overlays/dev` | Last. Each is its own Application, named after its folder |

Argo CD 1.8 dropped health for Applications, so the root would apply every wave at once. `infra/argocd/argo-cd-values.yaml` restores it in `argocd-cm` and adds one rule: a child whose last sync failed reports Degraded. A Degraded child fails the root's wave, so a failed migration Job holds the services back, and the root retries.

The waves order a bootstrap only. After that, each child syncs on its own when its folder changes. A tag bump and a migration in the same merge sync in parallel, so merge the migration first ([migrations](../k8s/migrations/README.md#under-argo-cd)).

## Values that stay out of Git

The repo is public, so nothing deployment-specific is committed ([Client IDs](../infra/modules/workload-identity/README.md#client-ids)). `infra/argocd` reads each value by name and sets it under `azure` in the root's Helm values. `values.schema.json` checks each one's shape: a GUID for every client ID and the subscription, `*.azurecr.io`, `*.vault.azure.net/`, `*.servicebus.windows.net`. The children pass them on:

| Value | From | Reaches | How |
| --- | --- | --- | --- |
| `acrLoginServer` | `azurerm_container_registry` | Every service image | `kustomize.images`: `ACR_LOGIN_SERVER/<service>=<registry>/<service>:*`. The `:*` keeps the tag the overlay pins |
| `keyVaultUri` | `azurerm_key_vault` | The ClusterSecretStore's `vaultUrl` | `kustomize.patches` |
| `subscriptionId`, `dnsZoneName`, `dnsZoneResourceGroup` | The root's variable, `azurerm_dns_zone` | ExternalDNS's flags and domain filter | Helm `valuesObject` |
| `serviceBusNamespace`, `serviceBusQueue` | `azurerm_servicebus_namespace`, `azurerm_servicebus_queue` | `SERVICEBUS_NAMESPACE` and `SERVICEBUS_QUEUE` on order, payment, shipping and worker | `kustomize.patches` |
| `serviceBusNamespaceName`, `serviceBusQueue` | The same | The worker's ScaledObject trigger | A JSON patch, with a `test` step that fails if the trigger moves |
| `clientIds` | `azurerm_user_assigned_identity`, one per identity | The four services' ServiceAccounts, and ExternalDNS, ESO and KEDA | `kustomize.patches` for ours, Helm `valuesObject` for the charts |

The values live in the root Application in the cluster and in the argocd state, never in Git. The bases keep their `*_FROM_ARGOCD` placeholders. An overlay applied by hand with `kubectl apply -k` gets placeholders, not real values.

## Sync policy

Every child, and the root, has:

- Automated sync with selfHeal. A merged change deploys with no click, and Argo CD reverts a hand edit to a managed field.
- Prune, except on data. A resource dropped from Git is deleted. data never prunes, so a bad commit can't delete Postgres, Redis or their disks. Delete a resource that left `k8s/data` by hand.
- Server-side apply. ESO's, KEDA's and cert-manager's CRDs are larger than the 256 KiB the last-applied annotation allows. From Argo CD 3.6, server-side apply also selects the server-side diff, which leaves out what mutating webhooks add.
- Retries, 5 with backoff from 10 seconds to 3 minutes, since a webhook from the wave before can take a moment to answer.
- SkipDryRunOnMissingResource on the Applications whose custom resources use another Application's CRDs: the store, data, migrations and the services.

### Testing a branch

The root, and every child that reads this repo, track `target_revision` in `infra/argocd`, `main` by default. To try a branch before merging, push it and apply the root with it:

```bash
tofu -chdir=infra/argocd apply -var target_revision=tc/my-branch
```

Every child follows the branch until the next apply without the variable. Chart versions come from the branch too. Apply again with no `-var` before merging, or the cluster stays on the branch.

## What Argo CD leaves alone

Nothing needs `ignoreDifferences` today:

- Replicas. Only the scheduler's StatefulSet sets `replicas`. The eight Deployments leave it out, so the HPAs and KEDA own the count.
- Reloader. It stamps an annotation on the pod template (`reloadStrategy: annotations`). The annotation isn't in Git, so it isn't in the diff.
- Secrets from ESO, and cert-manager's solver pods, Services and Ingresses. Their controllers create them. They carry no Argo CD tracking annotation, so Argo CD never prunes them.
- AKS's admissions enforcer, which adds expressions to every webhook's `namespaceSelector`. The server-side diff ignores what mutating webhooks add.
- The `default` StorageClass, which AKS owns. `k8s/storage` redeclares it with `Prune=false,Delete=false`.

selfHeal undoes any `kubectl edit`, `kubectl annotate` or `kubectl scale` on a field Git sets. That includes the old switch to the production certificate, now a commit ([apps](../k8s/apps/README.md#ingress)). `kubectl annotate ... force-sync=` on an ExternalSecret survives, since Git doesn't set that annotation.

## The UI

There is no Ingress. Port-forward and sign in as `admin`:

```bash
kubectl --context aks-v2-platform -n argocd port-forward svc/argocd-server 8080:443
# In another terminal: the password for admin.
kubectl --context aks-v2-platform -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

Open `https://localhost:8080`. The certificate is Argo CD's self-signed one. Each install generates a new password, so the nightly rebuild rotates it. For the CLI, keep the port-forward running and:

```bash
argocd login localhost:8080 --insecure --username admin \
  --password "$(kubectl --context aks-v2-platform -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d)"
```

## The evening destroy

`make down` destroys `infra/argocd` first, then platform.

1. The root's Helm release goes. The root has no `resources-finalizer`, so only the Application is deleted. The children and everything they deployed stay.
2. The argo-cd release goes. `crds.keep` leaves the CRDs, so the children aren't deleted with them.
3. The `argocd` namespace goes, and the children in it. None has a finalizer, so nothing waits for a controller that is already gone.
4. platform deletes the cluster, with everything still in it.

No step waits on a cascade, so the destroy never hangs on a finalizer.

On a live cluster, **removing a child from `argocd/apps` deletes its Application but leaves its resources running.** To remove a component, delete its resources first, or add `resources-finalizer.argocd.argoproj.io` to that child for one sync before removing it.

If platform was destroyed first, `tofu destroy` in `infra/argocd` can't reach the cluster. Its resources went with the cluster, so drop them from state: `tofu -chdir=infra/argocd state list`, then `state rm` each one.

## Rollback

Each service is its own Application, so a rollback touches none of the other eight. The schema stays expanded, since migrations only go forward ([Database migrations](../docs/architecture-decisions.md#database-migrations)).

### Standard: revert the tag bump

1. Find the commit that bumped `newTag` in `k8s/apps/<service>/overlays/dev/kustomization.yaml` and revert it on a branch:

   ```bash
   git log --oneline -3 -- k8s/apps/<service>/overlays/dev/kustomization.yaml
   git revert <commit>
   ```

2. Merge it. Auto-sync deploys the old tag within about three minutes, Argo CD's polling interval.
3. Watch it:

   ```bash
   kubectl --context aks-v2-platform -n argocd get application <service> -w
   kubectl --context aks-v2-platform -n apps rollout status deploy/<service> --timeout=10m
   ```

4. Scheduler only: a StatefulSet doesn't replace a pod that never turned Ready. If `scheduler-0` is unready from the broken release, delete it once the revert has synced. The controller recreates it from the reverted template ([Scheduler](../k8s/apps/README.md#scheduler-one-statefulset-not-cronjobs)):

   ```bash
   kubectl --context aks-v2-platform -n apps delete pod scheduler-0
   kubectl --context aks-v2-platform -n apps rollout status statefulset/scheduler --timeout=5m
   ```

### Faster: `argocd app rollback`, then revert

`argocd app rollback` redeploys an earlier sync from the Application's history without a merge. It refuses to run while auto-sync is on, and auto-sync would put Git's broken tag straight back. The root's selfHeal also manages each child's spec, so turning off auto-sync on the child alone lasts only until the root heals it. Turn it off on the root first:

```bash
argocd app set root --sync-policy none
argocd app set <service> --sync-policy none
argocd app history <service>
argocd app rollback <service> <history id>
```

Then revert the tag in Git as above and merge it. Once Git holds the good tag, turn auto-sync back on at the root. The root then restores the child's own policy from Git:

```bash
argocd app set root --sync-policy automated --auto-prune --self-heal
argocd app get <service>
```

Don't turn auto-sync back on before the revert has merged, or the broken tag redeploys. The scheduler step above applies here too. The next morning's `make up` resets the root's policy, since OpenTofu creates it.

## Proving it

These need a live cluster after `make up`. Run them from the repo root.

**P7-01 (#73): Argo CD runs on the system pool, has no Ingress, the UI answers and the repo is connected.**

```bash
K="kubectl --context aks-v2-platform"
$K -n argocd get pods -o wide
$K get ingress -A | grep -c argocd || true
$K get ns argocd --show-labels
$K -n argocd port-forward svc/argocd-server 8080:443 >/dev/null & PF=$!
sleep 3
curl -sk -o /dev/null -w '%{http_code}\n' https://localhost:8080/
argocd login localhost:8080 --insecure --username admin \
  --password "$($K -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d)"
argocd repo list
kill $PF
```

Expect every pod Running, `0` Ingresses, `pod-security.kubernetes.io/enforce=restricted`, `200`, and the repo with `STATUS` `Successful`.

**P7-02 (#74): syncing the root creates every child.**

```bash
kubectl --context aks-v2-platform -n argocd get applications \
  -o custom-columns=NAME:.metadata.name,WAVE:.metadata.annotations.argocd\\.argoproj\\.io/sync-wave,SYNC:.status.sync.status,HEALTH:.status.health.status
```

Expect 23 rows: `root`, which has no wave, and the 22 children with the waves in the table above.

**P7-03 (#75): a platform resource deleted by hand comes back.**

```bash
K="kubectl --context aks-v2-platform"
$K delete storageclass premium-zrs
sleep 30
$K get storageclass premium-zrs -o jsonpath='{.metadata.creationTimestamp} {.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}{"\n"}'
```

Expect a creation time from the last minute and `true`. Bound PVCs are unaffected, since a StorageClass is only read when a volume is provisioned.

**P7-04 (#76): a fresh cluster bootstraps with no manual step, in wave order.**

After a fresh `make up`, and nothing else:

```bash
kubectl --context aks-v2-platform -n argocd get applications -o json | jq -r '.items[]
  | select(.metadata.name != "root")
  | [(.metadata.annotations["argocd.argoproj.io/sync-wave"] | tonumber), .metadata.name,
     .status.operationState.startedAt, .status.sync.status, .status.health.status] | @tsv' | sort -k1,1n -k3
```

Expect every row `Synced` and `Healthy`, and the start times rising with the wave. Then check the services: `kubectl --context aks-v2-platform -n apps get deploy,statefulset`.

**P7-05 (#77): a merged tag bump deploys with no manual sync.**

Bump one service's tag (`make images`, `make pin-images`, commit, merge), then:

```bash
K="kubectl --context aks-v2-platform"
$K -n argocd get application order-service -w
$K -n argocd get application order-service -o jsonpath='{.status.operationState.operation.initiatedBy}{"\n"}{.status.sync.revision}{"\n"}'
$K -n apps get deploy order-service -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

Expect the Application to go OutOfSync, then Synced within about three minutes, `{"automated":true}`, the merge commit as the revision, and the new tag on the Deployment.

**P7-06 (#78):** the runbook is above. P9-05 exercises it.

## Validate

Offline, from the repo root:

```bash
helm lint argocd/apps -f argocd/apps/values.example.yaml
helm template root argocd/apps -n argocd -f argocd/apps/values.example.yaml > /tmp/apps.yaml
kubeconform -strict -summary -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
  /tmp/apps.yaml
# Name, wave, namespace, prune, selfHeal, sync options. Every row reads true for
# selfHeal, and prune is false on data only.
yq -N -r '[.metadata.name, .metadata.annotations."argocd.argoproj.io/sync-wave", .spec.destination.namespace,
  .spec.syncPolicy.automated.prune, .spec.syncPolicy.automated.selfHeal,
  (.spec.syncPolicy.syncOptions | join(","))] | @tsv' /tmp/apps.yaml | sort -k2,2n
# Without the values from OpenTofu the schema refuses to render.
helm template root argocd/apps -n argocd
```

Expect `Valid: 22, Invalid: 0, Errors: 0, Skipped: 0`, the 22 rows in wave order, and the last command to fail with `missing property 'azure'`.
