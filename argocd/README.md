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
3. The root syncs `argocd/apps` and creates the children wave by wave. Each child syncs itself, and the root moves on only when the whole wave is Healthy. On 26 Nov 2026 wave 3, `external-secrets-store`, started at 12:31:26 during a short Healthy spell of `external-secrets` before ESO settled at 12:33:30. It still converged with no manual step.

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

Argo CD 1.8 dropped health for Applications, so the root would apply every wave at once. `infra/argocd/argo-cd-values.yaml` restores it in `argocd-cm`, stricter than Argo CD's own. Argo CD's health for an Application skips hooks and missing resources, and a failing sync stays Running while it retries, so on its own it could pass `migrations` while the Job still runs. The check here passes a child only once its last sync has Succeeded, and then on the child's own health:

- No sync yet, as when the root has just created it: Progressing. A child that is already Synced, say one recreated over resources that stayed, has nothing to sync and goes by its health.
- A sync running or retrying: Progressing.
- A sync that Failed or errored: Degraded, which fails the root's wave, for every wave but the last.
- A sync that Succeeded: the child's health.

It never asks for Synced, since data doesn't prune and can stay OutOfSync. So a migration Job that is still running, retrying or failed holds the services back. The root then retries until the wave passes ([When a sync fails](#when-a-sync-fails)).

The root waits on a wave's health only before it starts the next wave, so the gate holds up to wave 8: the services wait on `migrations`, as the gate test proved. Wave 9, the services, is the last, so nothing waits on it. The root's sync ends Succeeded once they are applied, and a service that is still rolling out or Degraded shows in the root's health, not as a failed sync. On 26 Nov 2026 the root's sync Succeeded at 12:41:54 and the services turned Healthy at 12:44:11. In an earlier run the root read Synced, Degraded and Succeeded while two services were still Degraded just after creation.

The waves order a bootstrap only. After that, each child syncs on its own when its folder changes. The exception is a change to `k8s/migrations/job.yaml` alone: Argo CD leaves the hook out of sync status, so run `argocd app sync migrations` after it merges ([migrations](../k8s/migrations/README.md#under-argo-cd)). A tag bump and a migration in the same merge sync in parallel, so merge the migration first ([migrations](../k8s/migrations/README.md#under-argo-cd)).

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
- Server-side apply. ESO's, KEDA's and cert-manager's CRDs are larger than the 256 KiB the last-applied annotation allows. From Argo CD 3.6, server-side apply also selects the server-side diff, which leaves out fields Argo CD doesn't own.
- Retries without a limit, with backoff from 10 seconds to 3 minutes, picking up a new commit on the tracked branch as it lands (`refresh`). Auto-sync never retries a failed revision by itself, so without them a short Degraded spell, such as a webhook from the wave before still starting, would leave every later wave undeployed until the next commit. `migrations` stops after 2, since rerunning a failed SQL file only marks the version dirty again.
- SkipDryRunOnMissingResource on the Applications whose custom resources use another Application's CRDs: the store, data, migrations and the services.

### Testing a branch

The root, and every child that reads this repo, track `target_revision` in `infra/argocd`, `main` by default. To try a branch before merging, push it and apply the root with it:

```bash
tofu -chdir=infra/argocd apply -var target_revision=tc/my-branch
```

Every child follows the branch until the next apply without the variable. Chart versions come from the branch too. Apply again with no `-var` before merging, or the cluster stays on the branch.

**After changing `target_revision`, end the root's running operation.** An operation keeps the revision it started with, and each retry refreshes only that branch to its latest commit ([argo-cd#28794](https://github.com/argoproj/argo-cd/issues/28794)). With retries unlimited, an operation retrying a failed wave never ends, and auto-sync starts no new one while it runs. On 26 Nov 2026 the root stayed on the old branch for about 55 minutes and 20 retries after the apply. Ending the operation makes auto-sync start one on the new revision at once:

```bash
# Logged in to the argocd CLI, as in The UI below.
argocd app terminate-op root
# Or with no login.
kubectl --context aks-v2-platform -n argocd patch application root --type merge \
  -p '{"status":{"operationState":{"phase":"Terminating"}}}'
```

`argocd app terminate-op --core` reads the kube context's namespace, so unless that is `argocd` it fails with `configmap "argocd-cm" not found`.

**Then end any child operation still running on the old revision.** A child stuck retrying keeps its own captured revision after the root updates its spec, the app-of-apps case argo-cd#28794 describes, and every child but `migrations` retries without a limit. The root applies a child's new spec when its wave starts, then waits on that child's health. So once the root's new operation names a child in `waiting for healthy state of ...` and that child stays Progressing, end the children's running operations:

```bash
# Every child but root and migrations with an operation still Running.
# migrations stops after 2 retries by itself.
for app in $(kubectl --context aks-v2-platform -n argocd get applications -o json | jq -r '
    .items[] | select(.metadata.name != "root" and .metadata.name != "migrations")
    | select(.status.operationState.phase == "Running") | .metadata.name'); do
  echo "terminating $app"
  kubectl --context aks-v2-platform -n argocd patch application "$app" --type merge \
    -p '{"status":{"operationState":{"phase":"Terminating"}}}'
done
```

Auto-sync then starts each one again on its current spec. A child ended before the root reaches its wave restarts on its old spec, so if a later wave stalls the same way, run the loop again then. A fix or revert pushed to the branch already tracked needs none of this: the next retry picks it up, within the 3-minute backoff cap.

## What Argo CD leaves alone

Nothing needs `ignoreDifferences` today:

- Replicas. Only the scheduler's StatefulSet sets `replicas`. The eight Deployments leave it out, so the HPAs and KEDA own the count.
- Reloader. It stamps an annotation on the pod template (`reloadStrategy: annotations`). The annotation isn't in Git, so it isn't in the diff.
- Secrets from ESO, and cert-manager's solver pods, Services and Ingresses. Their controllers create them. They carry no Argo CD tracking annotation, so Argo CD never prunes them.
- AKS's admissions enforcer, which adds expressions to every webhook's `namespaceSelector` that lacks them. It writes them as its own field manager, `admissionsenforcer`, and the server-side diff ignores fields Argo CD doesn't own. cert-manager's values already declare them, so only ESO's webhooks carry the enforcer's fields. [Proving it](#proving-it) checks that cert-manager's and ESO's webhooks don't flap.
- The `default` StorageClass, which AKS owns. `k8s/storage` redeclares it with `Prune=false,Delete=false`.

selfHeal undoes any `kubectl edit`, `kubectl annotate` or `kubectl scale` on a field Git sets. That includes the old switch to the production certificate, now a commit ([apps](../k8s/apps/README.md#ingress)). `kubectl annotate ... force-sync=` on an ExternalSecret survives, since Git doesn't set that annotation.

## When a sync fails

The root and every child retry on their own, every 3 minutes at most, and pick up a fix as soon as it merges to the branch they track. A change of `target_revision` also needs the root's operation ended, and any child's still running on the old revision ([Testing a branch](#testing-a-branch)). To see why one is stuck:

```bash
# The operation's phase and message, and each resource's state.
argocd app get <app>
# The wave the root waits on.
argocd app get root
```

To retry now rather than wait for the backoff, stop the retrying operation and sync again, the child first, then the root. `terminate-op` fails harmlessly if nothing is running:

```bash
argocd app terminate-op <app>; argocd app sync <app>
argocd app terminate-op root; argocd app sync root
```

After a failed migration at bootstrap: clear the dirty version as in [When a migration fails](../k8s/migrations/README.md#when-a-migration-fails), merge the fixed file if the file was wrong, then run `argocd app sync migrations`. Once `migrations` is Healthy, the root's next retry starts the services, or sync the root as above.

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

On a live cluster, **removing a child from `argocd/apps` deletes its Application but leaves its resources running.** To remove a component, delete its resources first, or add `resources-finalizer.argocd.argoproj.io` to that child for one sync before removing it. Finish a removal the same day. A finalizer left on a child makes the evening's argocd destroy wait on a controller that is going away.

If the argocd destroy fails or is cancelled, `make down` stops, prints the recovery and says the platform destroy has not run, so the cluster is still up. For a stuck finalizer, strip it, then run `make down` again:

```bash
kubectl --context aks-v2-platform -n argocd patch application <name> --type merge -p '{"metadata":{"finalizers":null}}'
```

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

These need a live cluster after `make up`. Run them from the repo root. The commands work in bash, and in zsh with `setopt interactivecomments`, which oh-my-zsh sets. Without it, zsh reads each `#` line as a command, and an apostrophe in a comment opens a quote that swallows the lines after it.

**P7-01 (#73): Argo CD runs on the system pool, has no Ingress, the UI answers and the repo is connected.**

```bash
K() { kubectl --context aks-v2-platform "$@"; }
K -n argocd get pods -o wide
K get ingress -A | grep -c argocd || true
K get ns argocd --show-labels
# kubectl in full, not K: a backgrounded function runs in a subshell, so
# kill $PF would stop the subshell and leave the port-forward running.
kubectl --context aks-v2-platform -n argocd port-forward svc/argocd-server 8080:443 >/dev/null & PF=$!
sleep 3
curl -sk -o /dev/null -w '%{http_code}\n' https://localhost:8080/
argocd login localhost:8080 --insecure --username admin \
  --password "$(K -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d)"
argocd repo list
kill $PF
```

Expect every pod Running, `0` Ingresses, `pod-security.kubernetes.io/enforce=restricted`, `200`, and the repo with `STATUS` `Successful`.

Proven on 26 Nov 2026: the four Argo CD pods Running on `aks-system` nodes, `0` Ingresses, `enforce=restricted`, `200`, and the repo `Successful`.

**P7-02 (#74): syncing the root creates every child.**

```bash
kubectl --context aks-v2-platform -n argocd get applications \
  -o custom-columns=NAME:.metadata.name,WAVE:.metadata.annotations.argocd\\.argoproj\\.io/sync-wave,SYNC:.status.sync.status,HEALTH:.status.health.status
```

Expect 23 rows: `root`, which has no wave, and the 22 children with the waves in the table above.

Proven on 26 Nov 2026: 23 rows, every one Synced and Healthy.

**P7-03 (#75): a platform resource deleted by hand comes back.**

```bash
K() { kubectl --context aks-v2-platform "$@"; }
K delete storageclass premium-zrs
sleep 30
K get storageclass premium-zrs -o jsonpath='{.metadata.creationTimestamp} {.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}{"\n"}'
```

Expect a creation time from the last minute and `true`. Bound PVCs are unaffected, since a StorageClass is only read when a volume is provisioned.

Proven on 26 Nov 2026: `premium-zrs` was deleted at 12:45:20 and an automated sync recreated it in the same second, still the default. Both PVCs stayed Bound.

**P7-04 (#76): a fresh cluster bootstraps with no manual step, in wave order.**

After a fresh `make up`, and nothing else:

```bash
kubectl --context aks-v2-platform -n argocd get applications -o json | jq -r '.items[]
  | select(.metadata.name != "root")
  | [(.metadata.annotations["argocd.argoproj.io/sync-wave"] | tonumber), .metadata.name,
     .status.operationState.startedAt, .status.sync.status, .status.health.status] | @tsv' | sort -k1,1n -k3
```

Expect every row `Synced` and `Healthy`, and the start times rising with the wave. Then check the services: `kubectl --context aks-v2-platform -n apps get deploy,statefulset`.

Proven on 26 Nov 2026, with no manual step: 14 minutes from the root's first sync at 12:30:37 to every child Healthy at 12:44:11. The start times rose with the wave, from `node-pools` at 12:30:47 to the services at 12:41:52, and every operation was automated with a retry count of 0.

**P7-04 (#76): a failed migration at bootstrap holds the services back.**

On a test branch, add a migration that fails, then bootstrap a fresh cluster from that branch:

```bash
git switch -c tc/p7-gate-test
printf "SET lock_timeout = '10s';\nSELECT 1/0;\n" > k8s/migrations/sql/000002_fail.up.sql
yq -i '.configMapGenerator[0].files += ["sql/000002_fail.up.sql"]' k8s/migrations/kustomization.yaml
git add k8s/migrations && git commit -m "test: a migration that fails" && git push -u origin tc/p7-gate-test
make platform kubeconfig
tofu -chdir=infra/argocd apply -var target_revision=tc/p7-gate-test
```

About 15 minutes later:

```bash
K() { kubectl --context aks-v2-platform "$@"; }
K -n argocd get application migrations -o jsonpath='{.status.operationState.phase} {.status.operationState.message}{"\n"}'
K -n apps logs job/db-migrate --tail=5
K exec -n data postgres-0 -- psql -U postgres -d orders -tAc "SELECT version, dirty FROM schema_migrations"
K -n argocd get applications -o name | grep -c -E '/(api-gateway|order-service|inventory-service|payment-service|notification-service|shipping-service|dashboard-api|worker|scheduler)$'
K -n apps get deploy,statefulset
K -n argocd get application root -o jsonpath='{.status.operationState.phase} {.status.operationState.message}{"\n"}'
```

Expect `Failed` with `one or more synchronization tasks completed unsuccessfully (retried 2 times)`. The log usually reads `Dirty database version 2. Fix and force version.`: Argo CD recreates the hook Job on each retry, so the first run's `division by zero` is gone unless you catch it, and later runs stop at the dirty version. `2|t` from `schema_migrations` is the proof that 000002 failed. Then `0` service Applications, no workloads in `apps` (`No resources found in apps namespace.`), and the root `Running`, its message cycling through `waiting for healthy state of ...` for each wave, then `Retrying attempt #N`. On 26 Nov 2026 it read exactly that.

Then clean up. Set the version back to 1 ([When a migration fails](../k8s/migrations/README.md#when-a-migration-fails)), apply `infra/argocd` again to go back to `main`, or to the branch under test if this one came off it, then end the root's operation, and any child's still running on the gate branch, as in [Testing a branch](#testing-a-branch). Without that the root keeps retrying the gate branch. Within about 3 minutes `migrations` reports `no change` with the version `1|f`, and the nine services' Applications appear and turn Healthy. Delete the branch.

**P7-05 (#77): a merged tag bump deploys with no manual sync.**

Bump one service's tag (`make images`, `make pin-images`, commit, merge), then:

```bash
K() { kubectl --context aks-v2-platform "$@"; }
K -n argocd get application order-service -w
K -n argocd get application order-service -o jsonpath='{.status.operationState.operation.initiatedBy}{"\n"}{.status.sync.revision}{"\n"}'
K -n apps get deploy order-service -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

Expect the Application to go OutOfSync, then Synced within about three minutes, `{"automated":true}`, the merge commit as the revision, and the new tag on the Deployment.

Proven on 26 Nov 2026, with the bump pushed to the tracked branch, `tc/p7-argocd`, rather than merged, since P7 wasn't merged yet. order-service synced at 12:55:14, 3 minutes 17 seconds after the push at 12:51:57: about the 3-minute poll plus the sync. `{"automated":true}`, revision `f56ff06`, and the new image, `8770f95129aa`, rolled out.

**Server-side diff and AKS's admissions enforcer: the webhooks don't flap.**

```bash
K() { kubectl --context aks-v2-platform "$@"; }
# kubectl 1.36 leaves managedFields out unless asked.
for wh in cert-manager-webhook externalsecret-validate secretstore-validate; do
  printf '%s ' "$wh"
  K get validatingwebhookconfiguration "$wh" -o json --show-managed-fields | jq -c '[.metadata.managedFields[].manager] | unique'
done
for i in $(seq 10); do
  for app in cert-manager external-secrets; do
    K -n argocd get application $app -o jsonpath='{.metadata.name} {.status.sync.status} {.status.operationState.finishedAt}{"\n"}'
  done; sleep 30
done | sort | uniq -c
argocd app diff cert-manager; echo "exit $?"
argocd app diff external-secrets; echo "exit $?"
```

Expect `cert-manager-webhook ["argocd-controller","cert-manager-cainjector"]`: cert-manager's values already declare the AKS expressions, so the enforcer has nothing to add there. ESO's two webhooks read `["admissionsenforcer","argocd-controller","external-secrets"]`. Then one line per Application with a count of 10: `Synced` with the same finish time for five minutes, so no sync ran. Both diffs print nothing and exit 0.

Proven on 26 Nov 2026: those managers, 10 of 10 unchanged for both Applications, and both diffs empty with exit 0.

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
