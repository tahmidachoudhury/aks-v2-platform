# apps

**TL;DR:** The nine services run in the `apps` namespace. Each has a folder here named after its folder in `app/services/`, with a `base` and a `dev` overlay. One shared Kustomize component gives every workload the same pod shape. Each service is its own Argo CD Application, deployed after the migrations, which fills in the values kept out of Git. `make images` builds each service into ACR, tagged with the last commit that touched its folder. Key Vault supplies secrets through one ExternalSecret per service, and Reloader restarts only the services whose Secret changed.

Filled by P6-01 (#62), P6-02 (#63), P6-03 (#64), P6-04 (#65), P6-05 (#66), P6-06 (#67), P6-07 (#68), P6-08 (#69), P6-09 (#70), P6-10 (#71) and P6-11 (#72).

## Layout

```text
k8s/apps/
  components/service/       the pod shape every workload shares (a Kustomize Component)
  components/host-spread/   a hard node spread for the five multi-replica services
  <service>/base/           ServiceAccount, Deployment (the scheduler: StatefulSet), Service, NetworkPolicy, PodDisruptionBudget,
                            HorizontalPodAutoscaler, ExternalSecret, config.env
  <service>/overlays/dev/   image tag for dev (and the scheduler's one replica), what its Application deploys
```

This follows [data](../data/README.md#why-k8sdatacomponent): one folder per namespace, then one per component. The service folder name is also the name of its ServiceAccount, of the federated credential's subject (`system:serviceaccount:apps:<service>`), and of its Argo CD Application.

The [Manifest tooling](../../docs/architecture-decisions.md#manifest-tooling) ADR warned of nine near-identical bases and chose a Kustomize component. `components/service` holds what the services share. Each base includes it and keeps only its port, config, Secret and identity. The component patches every Deployment and the scheduler's StatefulSet by kind (`Deployment|StatefulSet`, a regular expression) and merges into the container called `app`. It carries only fields both kinds have. It also labels everything `app.kubernetes.io/part-of: order-fulfilment`, on resources and pod templates but never on selectors.

Each file writes its selectors out. Kustomize's `labels` with `includeSelectors` also rewrites `podSelector`s in NetworkPolicies, which would break any rule that names another service.

## The services

| Service | Port | Replicas | Identity | Secret keys | Postgres |
| --- | --- | --- | --- | --- | --- |
| api-gateway | 8080 | 2 to 4 | none | `JWT_SECRET`; `REDIS_URL` | no |
| order-service | 8081 | 2 to 3 | `id-order-service`, Service Bus Data Sender | `PGPASSWORD` | yes |
| inventory-service | 8082 | 2 to 3 | none | `PGPASSWORD` | yes |
| payment-service | 8083 | 2 to 3 | `id-payment-service`, Service Bus Data Sender | `PGPASSWORD` | yes |
| notification-service | 8084 | 1 to 2 | none | `PGPASSWORD` | yes |
| shipping-service | 8085 | 2 to 3 | `id-shipping-service`, Service Bus Data Sender | `PGPASSWORD`; `SHIPPING_WEBHOOK_SECRET` | yes |
| dashboard-api | 8086 | 1 to 2 | none | `PGPASSWORD` | yes |
| worker | 8090, health only | 1 to 5 | `id-worker`, Service Bus Data Receiver | none | no |
| scheduler | 8091, health only | 1 | none | `PGPASSWORD` | yes |

The replica ranges are the [Autoscaling](../../docs/architecture-decisions.md#autoscaling) minimums and maximums. See [Autoscaling](#autoscaling). The worker has no Service, since nothing calls it. The scheduler has a headless one only because a StatefulSet needs a governing Service. Each base's `config.env` holds the non-secret settings. It becomes a ConfigMap with a hash suffix, so an edit rolls the pods. Service URLs use the Service names in `apps`, such as `http://order-service:8081`.

`ADMIN_EMAILS` is unset on api-gateway on purpose. Login checks no password, so anyone who knows an admin email would get an admin token ([service catalogue](../../docs/service-catalogue.md#api-gateway)).

## The pod shape

Every Deployment, and the scheduler's StatefulSet, gets these from `components/service`:

| Setting | Value | Why |
| --- | --- | --- |
| User and group | 65532, `runAsNonRoot` | The images are distroless `static:nonroot` |
| Root filesystem | Read-only | The services write nothing to disk |
| Capabilities | All dropped, no privilege escalation | Pod Security `restricted`, which `apps` enforces |
| Seccomp | `RuntimeDefault` | As above |
| CPU request | 40m, no CPU limit | The per-pod budget in [namespaces](../namespaces/README.md#cpu-budget) |
| Memory | 64Mi request, 256Mi limit | The namespace defaults, set explicitly |
| `enableServiceLinks` | `false` | Stops `ORDER_SERVICE_PORT` and similar variables being injected for every Service in `apps` |
| Rollout | `RollingUpdate`, `maxUnavailable: 0`, `maxSurge: 1`, `progressDeadlineSeconds: 300` | Deployments only, from `rollout.yaml`. See [Rollouts](#rollouts) |
| Spread | Across zones, then nodes, `ScheduleAnyway`. Nodes are a hard rule for the five multi-replica services, from `components/host-spread` | See [Disruption and spread](#disruption-and-spread) |
| `preStop` | The kubelet's `sleep` action, 5s | Holds SIGTERM back while Cilium and Traefik drop the pod from their endpoints. The images have no shell to exec `sleep` in |
| `terminationGracePeriodSeconds` | 45 | The 5s preStop, then the HTTP services' 5s drain and 30s shutdown: 40s. The scheduler's 5s, 20s job wait and 5s stop: 30s |
| Startup probe | `/livez`, every 5s, 30 tries | The Postgres users ping the database for up to 120s before they listen |
| Liveness probe | `/livez`, every 10s, 3s timeout, 3 tries | Always 200 while the process runs, so a dependency outage never restarts a pod |
| Readiness probe | `/healthz`, every 5s, 3s timeout, 3 tries | 503 on SIGTERM, for Postgres, and for Service Bus on the worker. The Postgres ping has no timeout, so the probe's 3s bounds it |

Every container port is named `http`, so the probes find it.

## Values from OpenTofu

The repo is public, so nothing deployment-specific is committed ([Client IDs](../../infra/modules/workload-identity/README.md#client-ids)). The bases carry placeholders, and each service's Application in [`argocd/apps`](../../argocd/README.md#values-that-stay-out-of-git) fills them in from the root's values, which OpenTofu sets:

| Value | From | Replaces | How |
| --- | --- | --- | --- |
| `acrLoginServer` | The registry in persistent | `ACR_LOGIN_SERVER` at the front of every image | `kustomize.images`, with `:*` so the overlay's pinned tag stays |
| `serviceBusNamespace` | The Service Bus namespace in platform, as an FQDN | `SERVICEBUS_FQDN_FROM_ARGOCD` on order, payment, shipping and worker | A strategic merge patch on the Deployment |
| `serviceBusNamespaceName` | The same, as a name | `namespace` in the worker's ScaledObject trigger. KEDA adds `.servicebus.windows.net` itself | A JSON patch |
| `serviceBusQueue` | The queue in platform | `SERVICEBUS_QUEUE_FROM_ARGOCD` on the same four, and `queueName` in the ScaledObject | Both patches |
| `clientIds` | Each workload identity in persistent | `CLIENT_ID_FROM_ARGOCD` in `azure.workload.identity/client-id` on the four ServiceAccounts | A strategic merge patch |

The root's chart checks the shape of every value before it renders: a GUID for each client ID, `*.azurecr.io`, `*.servicebus.windows.net`. A missing or malformed value fails the render, so Argo CD never applies a placeholder or a `null`. An overlay applied by hand with `kubectl apply -k` still holds the placeholders, so don't.

## Images

`make images` builds each service with `az acr build` from `app/services/<service>`, for `linux/amd64`. ACR runs the build, so you need no local Docker.

- Tag: the first 12 characters of the last commit that touched `app/services/<service>`. A service with unchanged code keeps its tag.
- Immutable: if the tag is already in ACR, the target skips the build. It finds out by listing the registry's repositories (`az acr repository list`), then the service's tags (`az acr repository show-tags`), and matching the tag exactly. Both calls succeed when there is nothing to list, so a build starts only when the repository or tag is absent. Any error from either, such as no access to the registry, stops `make` with az's message. After each image, built or found, the target locks the tag with `az acr repository update --write-enabled false`, so pushing or deleting that tag again fails.
- Subscription: every `az` call passes `--subscription` with `subscription_id` from `infra/persistent/terraform.tfvars`, the root that holds the registry, so it works whatever your `az` default is.
- Clean tree: uncommitted or untracked changes in a service folder stop the target, since the tag would name a commit without them.
- Pinning: each dev overlay pins its tag under `images`. `make pin-images` sets every overlay to its current tag, offline, with `sed`, so the comments survive. Commit the result. In P8 CI bumps the same line.

The kubelet identity has AcrPull on the registry (P3-11), so no pod needs a pull secret.

`az acr build` needs ACR Tasks. Some subscription types turn Tasks off and fail with `TasksOperationsNotAllowed`. If yours does, build and push from a machine with Docker: `az acr login --subscription <subscription_id> -n <acr_name>`, then `docker build --platform linux/amd64 -t <acr_login_server>/<service>:<tag> app/services/<service>` and `docker push`.

## Secrets

Each service's ExternalSecrets read only the Key Vault secrets it uses, through the `azure-key-vault` ClusterSecretStore ([external-secrets](../external-secrets/README.md)). The pods read the Secrets with `envFrom`. No Secret manifest is in Git. They are split by how they rotate:

| Secret | Keys | Refresh |
| --- | --- | --- |
| `<service>-postgres`, for the seven Postgres users | `PGPASSWORD` | On `force-sync` only |
| `api-gateway-redis` | `REDIS_URL` | On `force-sync` only |
| `api-gateway` | `JWT_SECRET` | Hourly |
| `shipping-service` | `SHIPPING_WEBHOOK_SECRET` | Hourly |

The database passwords must reach the server before the services, so their ExternalSecrets use `refreshPolicy: OnChange` with `refreshInterval: "0"`: ESO syncs them once and then only when the object changes, which the `force-sync` annotation does. With the default `Periodic` policy, a zero interval means "never again", and the annotation would do nothing. The order is in [Rotating a password](../data/README.md#rotating-a-password). The JWT key and the webhook secret need nothing else to change first, so they refresh hourly.

- Postgres: `DATABASE_URL` sits in `config.env` with no password: `postgres://app@postgres.data.svc.cluster.local:5432/orders?sslmode=disable`. The ExternalSecret provides `PGPASSWORD` from `postgres-password`. All seven Postgres users import `lib/pq` v1.10.9, which reads `PGPASSWORD` from the environment, then lets the URL override it (`connector.go`, `NewConnector`). A URL with no password keeps the environment's. The migration Job uses the same pattern ([migrations](../migrations/README.md#how-a-run-works)). A password with `@`, `:` or `/` needs no escaping.
- Redis: go-redis has no environment fallback, so api-gateway's ExternalSecret builds `REDIS_URL` with a template: `redis://:<password>@redis.data.svc.cluster.local:6379/0`. It escapes the password with `urlquery`. The generated secrets are 48 letters and digits today (`infra/modules/keyvault`), so escaping only matters if that changes.
- Rotation: each workload lists exactly the Secrets it reads in `secret.reloader.stakater.com/reload`, such as `shipping-service-postgres,shipping-service`, so [Reloader](../reloader/README.md) restarts only the services that read a changed secret.

## Workload identity

order-service, payment-service, shipping-service and worker each have:

- A ServiceAccount named after the service, annotated `azure.workload.identity/client-id`. The federated credential in `infra/platform/main.tf` trusts `system:serviceaccount:apps:<service>`.
- The pod label `azure.workload.identity/use: "true"`. The AKS webhook then injects `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_FEDERATED_TOKEN_FILE` and the projected token.
- `SERVICEBUS_NAMESPACE` set to the namespace's FQDN and no `SERVICEBUS_CONNECTION_STRING`, so `DefaultAzureCredential` signs in with that token.

Each identity holds one role, on the `order-events` queue only: Data Sender for the three publishers, Data Receiver for the worker ([role matrix](../../infra/modules/workload-identity/README.md#role-matrix)). The other five services have no label and no annotation, so the webhook injects nothing into them. All nine ServiceAccounts set `automountServiceAccountToken: false`, since no service calls the API server. The webhook mounts its own projected token at `/var/run/secrets/azure/tokens`, separate from the API token, so the four callers keep their Azure sign-in. No service reads Key Vault. ESO does that with its own identity.

## Rollouts

P6-03 (#64). Every Deployment rolls one pod at a time, and keeps every old pod until the new one is Ready:

1. The Deployment adds one new pod (`maxSurge: 1`). It takes traffic only once `/healthz` passes, so a pod that never turns Ready takes none.
2. Only then does it remove an old pod (`maxUnavailable: 0`). That pod leaves the Service endpoints straight away. The 5s `preStop` sleep gives Cilium and Traefik time to stop sending it new connections.
3. Kubernetes sends SIGTERM. The HTTP services fail `/healthz` and keep serving for `SHUTDOWN_DRAIN_DELAY` (5s), then `server.Shutdown` waits up to 30s for requests in flight. The 45s grace period covers all of it.
4. If no new pod turns Ready for 5 minutes (`progressDeadlineSeconds: 300`), the rollout is marked `ProgressDeadlineExceeded` and Argo CD reports the service Degraded. The old pods keep serving. Revert the tag in Git and merge it. `kubectl rollout undo` doesn't help, since selfHeal puts Git's tag straight back. A broken scheduler release also needs `scheduler-0` deleted after the revert, since a StatefulSet doesn't replace an unready pod ([Scheduler](#scheduler-one-statefulset-not-cronjobs)). The runbook is in [argocd](../../argocd/README.md#rollback).

The worker loses no messages in a rollout. It receives in PeekLock mode, so a message it hasn't completed is locked, not removed. On SIGTERM it abandons what it received after the signal, and Service Bus redelivers those at once. A message it was handling when the pod died returns when its one-minute lock expires.

The scheduler is a StatefulSet, so the rollout fields live in a second component patch, `rollout.yaml`, that targets Deployments only. The StatefulSet's own rolling update deletes `scheduler-0` before it creates the new one, and has no progress deadline: Argo CD shows it Progressing until the pod turns Ready (see [Scheduler](#scheduler-one-statefulset-not-cronjobs)).

With replicas near the HPA maximums, roll one service at a time. Each rollout adds a 40m surge pod, and at the maximums the `apps` quota has room for only a few at once ([Quotas](#quotas)). A surge pod the quota refuses holds its rollout until the deadline fails it.

## Disruption and spread

P6-04 (#65). Every service except the scheduler has a PodDisruptionBudget with `maxUnavailable: 1` and `unhealthyPodEvictionPolicy: AlwaysAllow`:

- A drain or a NAP consolidation evicts one pod of a service, then waits until its replacement is Ready before evicting the next. A service with two or more replicas always keeps one serving.
- A service at one replica (notification-service and dashboard-api at their minimum, the worker when the queue is quiet) can still be evicted, so a single replica never blocks a drain. It is down until its replacement is Ready. The ADR accepts that for these three, since none is on the path a customer waits on.
- `minAvailable: 1` would block every drain of a single-replica service, so it isn't used.
- The scheduler has no budget. It is one replica, and a budget allowing no disruption would block every drain of its node. A StatefulSet never starts its replacement before it has gone, and a missed tick only delays the next run.

The shared component spreads each service's pods across zones, then across nodes, with `maxSkew: 1` and `whenUnsatisfiable: ScheduleAnyway`. `matchLabelKeys` adds the pod's own `app.kubernetes.io/name` and `pod-template-hash` to the selector, so one rule spreads each service, and each revision, on its own.

A preference isn't enough. On a fresh cluster the scheduler put all 14 app pods on the one `general` node, in one zone, and NAP never added a second: `ScheduleAnyway` is met on whatever nodes exist. So the five services that run two or more replicas at their minimum, api-gateway, order-service, inventory-service, payment-service and shipping-service, also include `components/host-spread`. It makes their hostname entry a hard rule, still with `maxSkew: 1`:

- `whenUnsatisfiable: DoNotSchedule`. A pod the rule holds back stays `Pending`. NAP runs Karpenter, which treats a hard spread as a requirement and starts a node for it ([Karpenter scheduling](https://karpenter.sh/docs/concepts/scheduling/#topology-spread)).
- `nodeTaintsPolicy: Honor`. Only nodes the pod tolerates count ([topology spread](https://kubernetes.io/docs/concepts/scheduling-eviction/topology-spread-constraints/#spread-constraint-definition)). With the default, `Ignore`, the three `aks-system` nodes, tainted `CriticalAddonsOnly`, and any spot node would count as nodes with none of the service's pods. The global minimum would stay 0, so each `general` node could hold only one pod of a service, and a third replica would stay `Pending`. Cordoned and NotReady nodes drop out too.
- `minDomains: 2`. With `Honor` and one `general` node, that node would be the only domain and the rule would always hold, so every pod would pack onto it, as before. Below two eligible nodes, Kubernetes takes the global minimum as 0, so the node takes one pod of a service and the next waits `Pending`, which makes NAP add the second node. From two nodes on it has no effect, so three or four replicas split 2 and 1 or 2 and 2. Karpenter always takes the global minimum for hostname as 0, since it can add a node, and honours `nodeTaintsPolicy` when it counts ([Karpenter source](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/controllers/provisioning/scheduling/topologygroup.go)).

The result is two `general` nodes, each with at least one pod of each of the five. `general` offers only 2 vCPU sizes, so its 4 vCPU limit always holds both ([node-pools](../node-pools/README.md#sizes-and-limits)). NAP consolidation won't merge them back: it removes a node only when its pods fit elsewhere under their rules. The zone spread stays a preference for all nine, and so does the hostname spread for the other four: notification-service and dashboard-api idle at one replica, the worker prefers spot, and the scheduler is one pod. Pod counts don't change, so the `apps` quota is as before.

Why the node and not the zone: the first version of this rule, which the P6 live run on 24 Nov 2026 tested, made the zone entry hard instead. The limit holds two nodes but can't choose their zones, so a hard zone rule could strand pods. If NAP put both nodes in one zone, for example a second node started for `platform` or `data` overflow with no zone rule to follow, the five's second replicas would need a zone the limit has no room for, and each would be stuck at one pod. Under the host rule the same-zone case no longer strands anything: the five still get two replicas on two nodes, and lose only the zone spread. A full first node still can, now on CPU rather than zone, as the full-node case below describes. The zone preference still counts: Karpenter tries it first when it adds the second node for the five, and the scheduler follows it when the nodes are in two zones. The host rule's own spread and drain results are from 25 Nov 2026, under criterion 11 in [Proving it](#proving-it).

The cost shows when only one `general` node is eligible: cordoned or drained, NotReady, or being disrupted by NAP. That node is then the only domain, the global minimum is 0, and each of the five runs at most one pod on it. A drained pod of the five stays `Pending` until the node is back, since the limit has no room for a third node while the drained one exists. The other copy keeps serving, so no service reaches zero, but the HPA can't add capacity either: a node that goes NotReady under load leaves each of the five on one pod until it returns. A service with two pods on the drained node, at three or four replicas, stalls the drain: its budget allows one pod down, and the first one's replacement is `Pending`. Drain with the five at their minimums, one pod per node each.

With two nodes, a full node can still strand a pod, because the rule, not free CPU, picks the node. api-gateway at three replicas, two on node A and one on B, can put a fourth only on B. If B has no CPU left and A has room, the pod stays `Pending`. NAP can't help: Karpenter counts a new node as the place for it, and the limit has no room for a third. A rollout's surge pods hit the same case, since `pod-template-hash` spreads each revision on its own: with one new pod on A, the next must go on B, and with `maxUnavailable: 0` the rollout fails at its 300s deadline. This is rarer than under the zone rule, which had the same case and the same-zone one besides, but it is real. The two nodes run close to full at the quotas ([namespaces](../namespaces/README.md#capacity)).

The same case can meet the morning bring-up. If `platform` and `data` fill node A, NAP starts B for the overflow, and `general` is then at 4 of 4 vCPUs. Each of the five puts its first pod on B, where there is room. Its second must go on A, since a second on B would make the skew 2, and A has no CPU left. Karpenter's answer is a third node, which the limit refuses. The five sit at one pod each, their rollouts hit `ProgressDeadlineExceeded`, and Argo CD reports their Applications Degraded, so the root's last wave fails and it keeps retrying. Under `ScheduleAnyway` both pods would have run on B. This is a possible failure, not one seen yet: on 25 Nov 2026 the first node had room for one pod of each. To recover, move CPU from A to B by hand:

1. `kubectl cordon` node A, so nothing new lands there.
2. Delete `platform` pods on A, such as one Traefik pod or the External Secrets and KEDA pods, until about 40m is freed for each of the five's `Pending` pods. Their Deployments recreate them on B. Leave `postgres-0` and `redis-0` alone.
3. Check the recreated pods are `Running` on B, then `kubectl uncordon` A. The five's `Pending` pods then take A, their rollouts finish, and Argo CD reports them Healthy.

This works only if B has room for what moves, which it does whenever `ScheduleAnyway` would have fitted the five there. If the two nodes can't hold the requests between them, no placement helps: lower a request or a quota. Deleting node A instead should also end with room, but it moves Postgres and Redis too, and NAP's drain waits on their budgets ([data](../data/README.md#scheduling-off-spot-and-safe-to-move)). Neither path has been tried on a live cluster yet.

NAP drift also can't replace a `general` node while both exist, as far as upstream Karpenter's source shows. Drift plans a replacement before it drains, and leaves the drifted node out of that plan, so a 2 vCPU replacement fits the limit on paper ([disruption/helpers.go](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/controllers/disruption/helpers.go#L62-L70)). The launch then checks only that the pool isn't already over its limit, and 4 of 4 isn't ([provisioner.go](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/controllers/provisioning/provisioner.go#L495-L504), [nodepool.go](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/apis/v1/nodepool.go#L196-L208)). So NAP asks Azure for a third 2 vCPU node while all 10 of the subscription's standard vCPUs are in use: 6 on the system pool, 4 on `general`. Expect the launch to fail on quota. When the failed replacement is deleted, NAP untaints the drifted node and tries again on a later disruption pass ([queue.go](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/controllers/disruption/queue.go#L240-L246), [L163-L187](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/controllers/disruption/queue.go#L163-L187)), with at most a short pause while the failed offering is marked unavailable. Upstream's drift back-off sits behind the `NodePoolDriftBackoff` feature gate, Alpha and off by default ([options.go](https://github.com/kubernetes-sigs/karpenter/blob/748b37cd5562a3a545faf738f48f3fff313e2190/pkg/operator/options/options.go#L161-L168)). While each attempt is in flight, the drifted node carries the `karpenter.sh/disrupted:NoSchedule` taint, so under `nodeTaintsPolicy: Honor` it drops out as a domain. The other node is then the only one, and new pods of the five, from an HPA scale-out or a rollout surge, wait `Pending` as they do during a drain. It can't drift the node out without a replacement either, since the five's pods on it can't move to a node that already has one of theirs. Drift comes from a change to the NodePool or AKSNodeClass, including a new node image. To move a node onto the new spec by hand, one at a time, `kubectl cordon` it and `kubectl delete nodeclaim` its NodeClaim. NAP drains it, the five's pods from it wait `Pending`, and once the VM is gone the limit has room and NAP starts a new node under the current spec for them. If the drain waits on `postgres-0` or `redis-0`, delete that pod as [data](../data/README.md#scheduling-off-spot-and-safe-to-move) describes. This is from the source, not yet seen live.

## Autoscaling

P6-09 (#70), per the [Autoscaling](../../docs/architecture-decisions.md#autoscaling) ADR:

- The seven HTTP services each have a HorizontalPodAutoscaler in their base, on CPU at 70% of the 40m request, so a pod scales out at about 28m. The minimums and maximums are the ADR's, in the table above.
- The worker scales with [KEDA](../keda/README.md) on the queue's active message count, 5 per replica, from 1 to 5. KEDA creates and owns the HPA `keda-hpa-worker`. The ScaledObject is in the worker's base, and its Application fills in the namespace's name and the queue.
- The scheduler stays at one replica, set in its dev overlay.

Only the scheduler's StatefulSet sets `replicas`, to 1. The eight Deployments leave it out. The autoscaler owns the count, and a count in Git would reset it on every apply. A new Deployment starts at 1 and its HPA raises it to the minimum within a few seconds. One catch on a live cluster: the first apply after a manifest drops `replicas` resets the Deployment to 1 for a moment. The cluster is rebuilt every morning, so this only matters on a cluster that ran an earlier version of these manifests.

Postgres connections set the low maximums, not CPU: each pod opens up to its `SetMaxOpenConns` ([Postgres connections](../data/README.md#postgres-connections)).

## Spot

P6-10 (#71). The worker is the only service that tolerates the `spot` NodePool's taint, as [node-pools](../node-pools/README.md#spot) rules. Every other service, payment-service and the scheduler included, has no toleration, so the taint keeps it on `general`'s on-demand nodes. Postgres and Redis also require on-demand nodes by affinity ([data](../data/README.md#spot)).

The worker loses nothing to an eviction. Azure promises 30 seconds' notice, but in the P6 live run the pod got about 1 second of its 45-second grace period before the node went. The worker shuts down in a few seconds when it gets the chance: it abandons what it received after SIGTERM, so Service Bus redelivers those at once. A message it was handling when the node went comes back when its one-minute lock expires. Orders never wait on the worker: order-service, payment-service and shipping-service publish and answer straight away, and the queue holds the events until a worker takes them.

The worker prefers spot (`preferredDuringScheduling`, weight 100) but doesn't require it:

- It lands on spot whenever a spot node has room. When NAP needs a new node for a pending worker, the `spot` pool's weight (50 against 10) makes it a spot node.
- If the scheduler finds room on a `general` node first, the worker runs there until it is next rescheduled. In the P6 live run KEDA's scale-outs landed on `general`, which had room. Spot is preferred, not guaranteed.
- If spot has no capacity, or the subscription's 3 spot vCPUs are used, the worker still runs on `general`. A hard requirement would leave the queue unworked until spot came back.

## Scheduler: one StatefulSet, not CronJobs

P6-11 (#72) asked which fits the code. The answer is a StatefulSet with one replica and no volumes:

- `app/services/scheduler/main.go` runs five jobs on in-process tickers (1, 5, 15, 30 and 60 minutes). Each runs once at startup, then on its tick. It is a long-running process with a health port.
- The jobs take no lock, so two copies would run every job twice. `expire_reservations` lowers `products.reserved` without checking that the reservation is still active, so two runs release the same stock twice ([Autoscaling](../../docs/architecture-decisions.md#autoscaling)).
- A Deployment's rolling update runs the old and new pods together, and the new one runs every job at startup. `strategy: Recreate` fixes rollouts only. On a drain, an eviction or a `kubectl delete pod`, a Deployment starts the new pod while the old one is still in its 40-second shutdown.
- A StatefulSet creates `scheduler-0` again only once the old `scheduler-0` has gone, whatever removed it. Its rolling update, with one replica, deletes the pod and waits for it before it creates the new one. If a node is lost without a clean shutdown, the pod isn't replaced until the node is confirmed gone, which is the trade the jobs need.
- The scheduler keeps no state, so it has no `volumeClaimTemplates`. The headless Service `scheduler` exists only because a StatefulSet requires `serviceName`.
- CronJobs would need a new entry point per job. `concurrencyPolicy: Forbid` also doesn't stop two Jobs overlapping across a CronJob edit. That change isn't worth making for five short jobs.

**A broken scheduler release needs its pod deleted after the revert.** With one replica, a StatefulSet rolls by deleting `scheduler-0` and creating it from the new template. If that pod never turns Ready, say from a bad image, reverting the tag in Git and applying again doesn't replace it, and neither does `kubectl rollout undo`: the controller waits for the broken pod to turn Ready first. Argo CD doesn't delete pods, so once the revert has synced, delete it by hand if it is unready and from the broken revision. The controller recreates it from the current template:

```bash
kubectl --context aks-v2-platform -n apps delete pod scheduler-0
kubectl --context aks-v2-platform -n apps rollout status statefulset/scheduler --timeout=5m
```

The cost: no scheduler copy runs while it rolls out or its node drains, and it has no PodDisruptionBudget or autoscaler. A missed tick only delays the next run, since each job works through whatever is due.

## Ingress

One standard `networking.k8s.io/v1` Ingress, `api-gateway`, in the api-gateway dev overlay, since its host belongs to the environment. It sends every path on `azure.order-fulfilment.tahmidchoudhury.uk`, the zone apex, to api-gateway on 8080 through Traefik (`ingressClassName: traefik`). The gateway does the routing the code expects:

| Path | Served by |
| --- | --- |
| `GET /` | dashboard-api's UI page, proxied by the gateway with no JWT check |
| `/auth/login`, `/auth/register` | The gateway itself |
| `/api/<service>/*` | The matching backend, after the rate limit and JWT check |
| `/api/shipping/webhook` | shipping-service, with no JWT check. The service checks `X-Webhook-Secret` |

ExternalDNS writes the A record for the host, pointing at Traefik's persistent IP. cert-manager's ingress-shim reads the `cert-manager.io/cluster-issuer` annotation and orders a certificate into the Secret `api-gateway-tls` with HTTP-01 through Traefik.

**Staging by default.** The overlay uses `letsencrypt-staging`, as the [TLS certificates](../../docs/architecture-decisions.md#tls-certificates) ADR and the [cert-manager rate limits](../cert-manager/README.md#rate-limits) rule. Production allows five certificates for this name in any seven days, and each morning's cluster orders a new one. Browsers distrust the staging certificate, but the whole flow runs.

**Production for a proof window**, a demo or grading run. Each switch uses one of the five. selfHeal reverts a `kubectl annotate` within seconds, so the switch is a commit: set `cert-manager.io/cluster-issuer: letsencrypt-production` in `api-gateway/overlays/dev/ingress.yaml` and merge it. Then wait for the Secret to name the production issuer. The old staging certificate stays Ready while the new one is ordered:

```bash
kubectl --context aks-v2-platform -n apps wait secret api-gateway-tls --timeout=5m \
  --for=jsonpath='{.metadata.annotations.cert-manager\.io/issuer-name}'=letsencrypt-production
```

cert-manager sees the issuer change and orders a production certificate into the same Secret. Revert the commit when the window ends, which orders a staging certificate again. A forgotten switch only lasts until the revert: each morning's cluster orders whatever Git says, so leave it on staging in Git.

The client's address survives the trip. Traefik's Service has `externalTrafficPolicy: Local`, so Traefik sees the real client, and it replaces any `X-Forwarded-For` the client sends with the address it saw ([traefik](../traefik/README.md)). The gateway trusts that header only from the pod CIDR in `TRUSTED_PROXIES`, and its NetworkPolicy lets only Traefik connect, so the rate limit counts each client, not Traefik (#128).

## NetworkPolicies

`apps` and `data` deny everything in and out by default (`default-deny` in [namespaces](../namespaces/README.md#networkpolicies)), and every pod may reach CoreDNS (`allow-dns`). Each service's base adds one NetworkPolicy for its own pods, with the calls the code makes and nothing more ([service catalogue](../../docs/service-catalogue.md)). Standard `networking.k8s.io/v1` policies, which Cilium enforces (`network_policy = "cilium"` in [aks](../../infra/modules/aks/README.md)).

| Pods | In from | Out to |
| --- | --- | --- |
| api-gateway | Traefik's pods in `platform`, on 8080 | The five backends and dashboard-api, Redis on 6379 |
| order-service, inventory-service | api-gateway and the scheduler, on their port | Postgres on 5432. order-service also Azure |
| payment-service, shipping-service | api-gateway, on their port | Postgres, Azure |
| notification-service, dashboard-api | api-gateway, on their port | Postgres |
| worker | Nothing | Azure |
| scheduler | Nothing | Postgres, order-service on 8081, inventory-service on 8082 |
| HTTP-01 solver (`acme.cert-manager.io/http01-solver`) | Traefik, on 8089 | Nothing |

The other side of each pair is in the same place: Postgres and Redis allow their callers in `data` ([data](../data/README.md#networkpolicies)), the migration Job's policy lets it out to Postgres ([migrations](../migrations/README.md)), and the snapshot CronJob's lets it reach the API server ([backup](../backup/README.md)).

- **Azure** is Entra ID for the workload identity token exchange and Service Bus, on 443, 5671 and 5672. Both are public endpoints, so the rule allows `0.0.0.0/0` on those ports, minus the private ranges. Naming the hosts would need FQDN policies, which on AKS come with Advanced Container Networking Services, a paid add-on.
- **The solver.** cert-manager starts HTTP-01 solver pods in the Ingress's namespace, `apps`. Without the solver rule, default-deny drops Let's Encrypt's challenge at the pod and no certificate is ever issued.
- **The worker** calls no service today: its handlers only log. When they start calling services, add an egress rule here and an ingress rule on each service it calls.
- **Labels are the boundary.** A pod labelled `app.kubernetes.io/name: api-gateway` gets the gateway's access. Only those who can create pods in `apps` can do that, so RBAC on pod creation is the control that backs this up.
- **`platform` has no policies yet.** Traefik, cert-manager, ExternalDNS, External Secrets and Reloader talk freely. Adding default deny there is a follow-up.

Kubelet probes come from the node, not a pod. Cilium lets a node reach its own pods by default (`allow-localhost`), so default deny leaves the probes working. The live checks below confirm it, since every pod would go unready if it didn't.

## Quotas

### apps

Every service pod requests 40m CPU and 64Mi, with a 256Mi limit. The migration Job requests 10m and 32Mi with a 64Mi limit. cert-manager's HTTP-01 solver pod requests 10m and 64Mi with a 64Mi limit, and runs only while a certificate is ordered.

| | Pods | `requests.cpu` | `requests.memory` | `limits.memory` |
| --- | --- | --- | --- | --- |
| Minimums: 2+2+2+2+2+1+1+1+1 | 14 | 560m | 896Mi | 3,584Mi |
| Maximums: 4+3+3+3+3+2+2+5+1 | 26 | 1,040m | 1,664Mi | 6,656Mi |
| One rolling-update surge pod | 1 | 40m | 64Mi | 256Mi |
| Migration Job | 1 | 10m | 32Mi | 64Mi |
| HTTP-01 solver | 1 | 10m | 64Mi | 64Mi |
| **At the maximums, with all three** | **29 of 40** | **1,100m of 1,250m** | **1,824Mi of 4Gi** | **7,040Mi of 10Gi** |

It holds. The 150m left is room for three more surge pods, so at the maximums at most four services can roll at once. At the minimums, all eight rolling Deployments can roll together (22 pods, 880m). That is why the [Rollouts](#rollouts) section says to roll one service at a time when replicas are high. The load test runs in `default`, outside this quota.

### platform quota

| Component | Pods | CPU requests | Memory requests | Memory limits |
| --- | --- | --- | --- | --- |
| Traefik | 2 | 100m | 128Mi | 512Mi |
| cert-manager | 3 | 30m | 160Mi | 640Mi |
| External Secrets | 4 | 40m | 160Mi | 640Mi |
| ExternalDNS | 1 | 10m | 32Mi | 128Mi |
| Reloader | 1 | 10m | 32Mi | 128Mi |
| KEDA operator and metrics server | 2 | 40m | 160Mi | 512Mi |
| **Total** | **13 of 20** | **230m of 500m** | **672Mi of 2Gi** | **2,560Mi of 4Gi** |

While it runs, cert-manager's startupapicheck Job adds one pod, 50m, 64Mi and a 256Mi limit, from the LimitRange defaults.

## Morning apply

Argo CD deploys the nine services last, in wave 9, after the `migrations` sync has succeeded, so a migration still running, or one that failed, holds every service back ([argocd](../../argocd/README.md#how-the-cluster-comes-up)). Each service's Application waits for its ExternalSecrets and its rollout to turn Healthy. The images must already be in ACR. Run `make images` once after a service changes, not every morning, since ACR outlives the nightly destroy. A merged tag bump deploys on its own, with no click.

## Proving it

These need a live cluster after `make up`. Run them from the repo root. The commands work in bash, and in zsh with `setopt interactivecomments`, which oh-my-zsh sets. Without it, zsh reads each `#` line as a command, and an apostrophe in a comment opens a quote that swallows the lines after it. The `az` commands name their subscription, so set both first. The registry and the vault are in the persistent root's, the Service Bus queue and the node VMs in the platform root's:

```bash
SUB=$(awk -F'"' '/^[[:space:]]*subscription_id[[:space:]]*=/ {print $2; exit}' infra/persistent/terraform.tfvars)
PLATFORM_SUB=$(awk -F'"' '/^[[:space:]]*subscription_id[[:space:]]*=/ {print $2; exit}' infra/platform/terraform.tfvars)
```

**P6-02 (#63), criterion 3: nine images in ACR, pinned by the overlays.**

```bash
make images
ACR=$(tofu -chdir=infra/persistent output -raw acr_name)
for svc in api-gateway order-service inventory-service payment-service notification-service \
           shipping-service dashboard-api worker scheduler; do
  tag=$(sed -n 's/^    newTag: //p' k8s/apps/$svc/overlays/dev/kustomization.yaml)
  az acr repository show --subscription "$SUB" --name "$ACR" --image "$svc:$tag" --query name -o tsv
done
```

Expect nine `<service>:<tag>` lines and no error. Every tag matches `git log -1 --format=%H -- app/services/<service>`.

**Criterion 4: all nine Ready after the migration.**

```bash
kubectl --context aks-v2-platform -n apps get deploy,sts,pods -l app.kubernetes.io/part-of=order-fulfilment
```

Expect eight Deployments and the scheduler's StatefulSet at their replica counts, every pod `Running` and `1/1` Ready, with no restarts.

**P6-05 (#66), criterion 5: each publisher sends and the worker receives with its own identity. The rest get no Azure token.**

```bash
CTX=aks-v2-platform
# The webhook injected the identity into the four, and only those.
kubectl --context $CTX -n apps get pods -o json | jq -r '.items[] |
  [.metadata.labels["app.kubernetes.io/name"],
   ([.spec.containers[0].env[]?.name | select(startswith("AZURE_"))] | join(","))] | @tsv' | sort -u

# The Azure token is mounted on the four, and the API token on none.
kubectl --context $CTX -n apps get pods -o json | jq -r '.items[] |
  [.metadata.labels["app.kubernetes.io/name"], ([.spec.containers[0].volumeMounts[]?.mountPath] | join(","))] | @tsv' | sort -u

# Each sees its own client ID. No value is printed, only a match per service.
for svc in order-service payment-service shipping-service worker; do
  want=$(tofu -chdir=infra/persistent output -json workload_identity_client_ids | jq -r --arg s $svc '.[$s]')
  got=$(kubectl --context $CTX -n apps get pod -l app.kubernetes.io/name=$svc -o jsonpath='{.items[0].spec.containers[0].env[?(@.name=="AZURE_CLIENT_ID")].value}')
  [[ "$want" == "$got" ]] && echo "$svc: own identity" || echo "$svc: MISMATCH"
done

# Readiness reports Service Bus ok on the publishers.
kubectl --context $CTX -n apps port-forward deploy/order-service 18081:8081 >/dev/null & PF=$!
sleep 2; curl -s localhost:18081/healthz; kill $PF

# Send through the gateway, then watch the worker receive it.
TOKEN=$(curl -s -X POST https://azure.order-fulfilment.tahmidchoudhury.uk/auth/login \
  -H 'Content-Type: application/json' -d '{"email":"p6@example.com","password":"x"}' | jq -r .token)
ORDER=$(curl -s -X POST https://azure.order-fulfilment.tahmidchoudhury.uk/api/orders/ \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"items":[{"product_id":"p1","quantity":1,"price":1.5}],"currency":"GBP"}' | jq -r .id)
echo "order $ORDER"

# payment-service and shipping-service publish too, but only for role admin or
# service. The gateway issues neither while ADMIN_EMAILS is unset, so call them
# directly with the headers an internal caller sends.
kubectl --context $CTX -n apps port-forward deploy/payment-service 18083:8083 >/dev/null & PF=$!
sleep 2
curl -s -X POST localhost:18083/charge -H 'X-User-Role: service' -H 'X-User-Email: p6@example.com' \
  -H 'Content-Type: application/json' \
  -d '{"order_id":'"$ORDER"',"customer_id":"p6@example.com","amount":1.5,"currency":"GBP","method":"card"}'; echo
kill $PF
kubectl --context $CTX -n apps port-forward deploy/shipping-service 18085:8085 >/dev/null & PF=$!
sleep 2
curl -s -X POST localhost:18085/shipments -H 'X-User-Role: service' -H 'X-User-Email: p6@example.com' \
  -H 'Content-Type: application/json' \
  -d '{"order_id":'"$ORDER"',"recipient_name":"P6 check","city":"London","postcode":"SW1A 1AA"}'; echo
kill $PF

kubectl --context $CTX -n apps logs deploy/worker --since=2m | grep -E 'Processing event|Successfully processed'
```

Expect `AZURE_AUTHORITY_HOST,AZURE_CLIENT_ID,AZURE_FEDERATED_TOKEN_FILE,AZURE_TENANT_ID` on order, payment, shipping and worker, and nothing on the other five. The mount check shows `/var/run/secrets/azure/tokens` on those four and `/var/run/secrets/kubernetes.io/serviceaccount` on none of the nine. Then expect `own identity` four times, `"servicebus":"ok"` from order-service, an order number, a payment (`completed`, or `failed` one time in ten, which publishes as well), and a shipment with a tracking number. The worker's log then shows `Processing event` and `Successfully processed` for `order.created`, for `payment.completed` or `payment.failed`, and for `shipment.created`: all three publishers sent with their own identities. The direct calls are needed because `X-User-Role` comes from the token, and the gateway issues only `customer` while `ADMIN_EMAILS` is unset, so no request through it may charge or ship. The HTTPS calls need the Ingress from P6-07. Before then, port-forward to api-gateway and use `http://localhost:18080`. The order body follows `createOrder` in `app/services/order-service/main.go`.

The other five have no token file and no `AZURE_*` variable, so the SDK has nothing to sign in with. The images have no shell, so the check reads the pod spec instead of using exec.

**P6-06 (#67), criterion 6: JWT_SECRET comes from Key Vault, and no Secret is in Git.**

```bash
git grep -nE '^kind: Secret$' -- k8s || echo "no Secret manifests"
kubectl --context aks-v2-platform -n apps get externalsecret
VAULT=$(tofu -chdir=infra/persistent output -raw key_vault_name)
diff <(kubectl --context aks-v2-platform -n apps get secret api-gateway -o jsonpath='{.data.JWT_SECRET}' | base64 -d | shasum -a 256) \
     <(az keyvault secret show --subscription "$SUB" --vault-name "$VAULT" -n jwt-secret -o json | jq -j .value | shasum -a 256) \
  && echo "match"
```

Expect `no Secret manifests`, eleven ExternalSecrets `SecretSynced` (seven `-postgres`, `api-gateway`, `api-gateway-redis`, `shipping-service` and `db-migrate`), and `match`. Neither value is printed. Run the `az` call from an IP the vault firewall allows. The rotation test is in [reloader](../reloader/README.md#proving-it). It rotates `shipping-webhook-secret` and expects only shipping-service to restart.

**P6-07 (#68), criterion 8: the dashboard over HTTPS with a valid production certificate, `/api/*` through the gateway, and a forged `X-Forwarded-For` doesn't reset the rate limit.**

Switch to production first, as in [Ingress](#ingress). Then:

```bash
HOST=azure.order-fulfilment.tahmidchoudhury.uk
dig +short "$HOST"
tofu -chdir=infra/persistent output -raw ingress_ip_address; echo
# A trusted certificate: plain curl, no -k.
curl -sS -o /dev/null -w '%{http_code} %{content_type}\n' "https://$HOST/"
# -text, not -ext: macOS's LibreSSL openssl has no -ext.
CERT=$(echo | openssl s_client -connect "$HOST:443" -servername "$HOST" 2>/dev/null)
openssl x509 -noout -issuer -subject -enddate <<<"$CERT"
openssl x509 -noout -text <<<"$CERT" | grep 'DNS:'

# /api/* reaches the gateway: no token is a 401 from it, a token gets the orders.
curl -sS -o /dev/null -w '%{http_code}\n' "https://$HOST/api/orders/"
TOKEN=$(curl -s -X POST "https://$HOST/auth/login" -H 'Content-Type: application/json' \
  -d '{"email":"p6-rate@example.com","password":"x"}' | jq -r .token)
curl -sS -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" "https://$HOST/api/orders/"

# 110 requests, each with a different forged X-Forwarded-For. Wait a minute
# first, so the earlier calls have left the window.
sleep 60
for i in $(seq 110); do
  curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" \
    -H "X-Forwarded-For: 203.0.113.$((i % 250))" "https://$HOST/api/orders/"
done | sort | uniq -c
```

Expect the `dig` answer to equal the ingress IP. Then `200 text/html`, an issuer from Let's Encrypt (not `(STAGING)`), the host as subject, and `DNS:` followed by the host, the certificate's subject alternative name. `401`, then `200`. Last, `100 200` and `10 429`: the gateway counted every request against the one client address, whatever the header said. Revert the switch when the window ends.

**P6-08 (#69), criterion 9: default deny in `apps` and `data`. A pod with no allow rule reaches nothing. Only api-gateway reaches the backends and dashboard-api, plus the scheduler to order-service and inventory-service.**

The services' images have no shell, so the tests use `agnhost`, the Kubernetes e2e test image, either as a pod of its own or as a debug container inside a service's pod, sharing that pod's network and its policies. `restricted` Pod Security needs the security context in `np-test.json`.

```bash
CTX=aks-v2-platform
K() { kubectl --context "$CTX" "$@"; }
IMG=registry.k8s.io/e2e-test-images/agnhost:2.33
cat > /tmp/np-test.json <<'EOF'
{"securityContext":{"runAsNonRoot":true,"runAsUser":65532,"allowPrivilegeEscalation":false,
 "capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}
EOF
K get networkpolicy -n apps; K get networkpolicy -n data

# 1. A pod with no allow rule, in apps.
K -n apps run np-test --image=$IMG --restart=Never --labels=app.kubernetes.io/name=np-test   --overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}},
  "containers":[{"name":"np-test","image":"'$IMG'","args":["pause"],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}'
K -n apps wait pod/np-test --for=condition=Ready --timeout=2m
for t in postgres.data.svc.cluster.local:5432 redis.data.svc.cluster.local:6379 \
         api-gateway:8080 order-service:8081 dashboard-api:8086; do
  printf '%s ' "$t"; K -n apps exec np-test -- /agnhost connect "$t" --timeout=3s --protocol=tcp || true; echo
done
K -n apps delete pod np-test

# 2. From inside a service's pod: what it may and may not reach.
# Usage: probe SERVICE TARGET...
probe() {
  local rc=0
  pod=$(K -n apps get pod -l app.kubernetes.io/name=$1 -o name | head -1); shift
  for t in "$@"; do
    c=np-$RANDOM
    printf '%s -> %s: ' "$pod" "$t"
    K -n apps debug "$pod" -q -c "$c" --image=$IMG --profile=restricted --custom=/tmp/np-test.json \
      --attach -- /agnhost connect "$t" --timeout=3s --protocol=tcp
    # kubectl debug --attach exits 0 whatever the container did. Its exit code is in the pod status.
    code=
    for i in $(seq 20); do
      code=$(K -n apps get "$pod" -o json | jq -er --arg c "$c" \
        '.status.ephemeralContainerStatuses[] | select(.name == $c) | .state.terminated.exitCode') && break
      sleep 1
    done
    # Only a numeric exit code is a verdict. Empty or null means the container
    # never ran or never finished, so the target wasn't tested.
    case "$code" in
      0) echo ok ;;
      '' | *[^0-9]*) echo "unknown (exit ${code:-none})"; rc=1 ;;
      *) echo "blocked (exit $code)" ;;
    esac
  done
  return $rc
}
# Each probe returns 1 if any of its targets printed unknown. Collect them, so
# one failure early on isn't hidden by later probes that pass.
bad=0
probe api-gateway order-service:8081 dashboard-api:8086 redis.data.svc.cluster.local:6379 || bad=1
probe scheduler order-service:8081 inventory-service:8082 payment-service:8083 || bad=1
probe worker order-service:8081 postgres.data.svc.cluster.local:5432 || bad=1
probe order-service payment-service:8083 redis.data.svc.cluster.local:6379 || bad=1
[[ $bad == 0 ]] && echo "every probe ran" || echo "FAILED: a target printed unknown and wasn't tested"
```

Expect a NetworkPolicy per service plus `db-migrate`, `default-deny`, `allow-dns` and `acme-http01-solver` in `apps`, and `default-deny`, `allow-dns`, `postgres`, `redis` and `postgres-snapshot` in `data`. From `np-test`, every target prints `TIMEOUT`. A refused or reset connection would mean the policy let it through. Then `ok` for api-gateway to order-service, dashboard-api and Redis, and for the scheduler to order-service and inventory-service. `TIMEOUT` and `blocked (exit 1)` for the scheduler to payment-service, the worker to order-service and Postgres, and order-service to payment-service and Redis. The verdict comes from the debug container's exit code in `ephemeralContainerStatuses`, not from `kubectl debug`, which exits 0 whatever the container did, so `&& echo ok` would print `ok` every time. No line may read `unknown`: that means the debug container never ran or never finished within 20 seconds, for example on an image pull failure, and the target wasn't tested. `probe` then returns 1, and the block ends with `FAILED` instead of `every probe ran`. Name resolution works throughout, which proves `allow-dns`. Ephemeral debug containers can't be removed. They go when the pod is next replaced, by a rollout or `kubectl rollout restart`.

Proven on 25 Nov 2026: every policy listed, every line as expected, no `unknown`, and `every probe ran`. `kubectl debug` sometimes warned that it couldn't attach and fell back to streaming logs. That is harmless, since the verdict comes from the exit code.

Also check that the data namespace still works under default deny: `kubectl -n data create job snap-test --from=cronjob/postgres-snapshot` completes, which proves the API server rule. `make restore-drill` passes, and `argocd app sync migrations` reports `no change` in the Job's log. Delete `snap-test` after.

**P6-03 (#64), criterion 10: a rollout under load has no errors, a broken image fails without taking traffic, and rolling the worker loses no messages.**

Offline, the settings render as described: run [Validate](#validate) and check the second `yq` table. Live, run the load test from [loadtest](../loadtest/README.md) in one terminal, then in another. Step 2 uses the `argocd` CLI, so log in first as in [argocd](../../argocd/README.md#the-ui):

```bash
CTX=aks-v2-platform
K() { kubectl --context "$CTX" -n apps "$@"; }
# 1. Roll two services under load.
K rollout restart deploy/api-gateway deploy/order-service
K rollout status deploy/api-gateway --timeout=5m
K rollout status deploy/order-service --timeout=5m

# 2. A broken image: the new pod never turns Ready, the old ones keep serving.
# selfHeal would put Git's image back within seconds, so turn auto-sync off
# for the test, the root first (argocd/README.md, Rollback).
argocd app set root --sync-policy none
argocd app set payment-service --sync-policy none
ACR=$(tofu -chdir=infra/persistent output -raw acr_login_server)
K set image deploy/payment-service app="$ACR/payment-service:does-not-exist"
K rollout status deploy/payment-service --timeout=6m; echo "exit $?"
K get deploy payment-service -o jsonpath='{.status.conditions[?(@.type=="Progressing")].reason}{"\n"}'
K get endpointslices -l kubernetes.io/service-name=payment-service -o json \
  | jq -r '.items[].endpoints[] | [.targetRef.name, .addresses[0], "ready=\(.conditions.ready)"] | @tsv'
K rollout undo deploy/payment-service
# Back on: the root restores payment-service's own policy from Git.
argocd app set root --sync-policy automated --auto-prune --self-heal

# 3. Roll the worker while orders flow, then check nothing is left or dead-lettered.
K rollout restart deploy/worker
K rollout status deploy/worker --timeout=5m
# After the load test ends and the worker drains the queue:
az servicebus queue show --subscription "$PLATFORM_SUB" -g "$(tofu -chdir=infra/platform output -raw resource_group_name)" \
  --namespace-name "$(tofu -chdir=infra/platform output -raw servicebus_namespace_name)" \
  -n order-events --query countDetails
```

Expect the load test to finish with `http_req_failed` at 0% and exit 0 through steps 1 and 3. In step 2, `rollout status` exits 1 after about 5 minutes with `ProgressDeadlineExceeded`, the EndpointSlice lists the old pods with `ready=true` and the broken pod with `ready=false`, and the load test still sees no errors. The slice holds every pod the Service selects. Traefik and Cilium send traffic only to endpoints marked ready, so the broken pod takes none. In step 3, `activeMessageCount` and `deadLetterMessageCount` are both 0.

Proven on 25 Nov 2026 under the host rule, with `make loadtest DURATION=15m` at the default `RATE=10`. `http_req_failed` was 0.00% on all three k6 pods, 0 failed out of 18,904, 18,904 and 18,901 requests, and `make loadtest` exited 0. The api-gateway and order-service rollouts finished with their new pods one per node, and a pod watch every 6 seconds never saw a `Pending` pod. The broken payment-service image hit `ProgressDeadlineExceeded` after 5 minutes, with the broken pod `ready=false` and the two old pods `ready=true`. The worker rolled, and the queue then read 0 active and 0 dead-lettered messages. At `RATE=10` no HPA scaled out, so every service stayed at its minimum. A surge pod for a service at three or more replicas, the case in [Disruption and spread](#disruption-and-spread) that can strand, was not exercised.

**P6-04 (#65), criterion 11: the five multi-replica services run on at least two nodes, and draining a node never takes one to zero Ready.**

```bash
CTX=aks-v2-platform
K() { kubectl --context "$CTX" "$@"; }
FIVE='app.kubernetes.io/name in (api-gateway,order-service,inventory-service,payment-service,shipping-service)'
# 1. Two general nodes, and each of the five on both. Zones are reported, not required.
K get nodes -l karpenter.sh/nodepool=general -L topology.kubernetes.io/zone,node.kubernetes.io/instance-type
K get nodes -o json > /tmp/nodes.json
K -n apps get pods -l "$FIVE" -o json | jq -r --slurpfile n /tmp/nodes.json '
  ($n[0].items | map({key: .metadata.name, value: .metadata.labels["topology.kubernetes.io/zone"]}) | from_entries) as $zone
  | [.items[] | select(.spec.nodeName) | {svc: .metadata.labels["app.kubernetes.io/name"], node: .spec.nodeName, zone: $zone[.spec.nodeName]}]
  | group_by(.svc)[] | ([.[].node] | unique | length) as $h | ([.[].zone] | unique) as $z
  | "\(.[0].svc) nodes: \($h), zones: \($z | length) (\($z | join(" ")))"'
K -n apps get pods -l "$FIVE" --field-selector=status.phase=Pending

# 2. Drain one general node. The selector leaves postgres-0 and redis-0 in
# place: their budgets refuse every eviction, so a plain drain would retry
# them until it timed out.
K -n apps get pdb
NODE=$(K get nodes -l karpenter.sh/nodepool=general -o jsonpath='{.items[0].metadata.name}')
# Ready replicas of the five multi-replica services, every second, while the node drains.
( for i in $(seq 240); do
    K -n apps get deploy api-gateway order-service inventory-service payment-service shipping-service \
      -o jsonpath='{range .items[*]}{.metadata.name}={.status.readyReplicas} {end}{"\n"}'; sleep 1
  done > /tmp/ready.log ) &
K drain "$NODE" --ignore-daemonsets --delete-emptydir-data \
  --pod-selector='app.kubernetes.io/name notin (postgres,redis)' --timeout=10m
K -n apps get pods -l "$FIVE" -o wide --field-selector=status.phase=Pending
wait
grep -E '=0( |$)|=( |$)' /tmp/ready.log || echo "never zero"
K uncordon "$NODE"
K -n apps get pods -l "$FIVE" --field-selector=status.phase=Pending
```

Expect two `general` nodes, `Standard_D2s_v6` or `Standard_D2nls_v6`, usually in different zones. Each of the five shows `nodes: 2`, and none is `Pending`. It usually shows `zones: 2` too, but `zones: 1` is allowed under the host rule: note which you got. Then each budget at `ALLOWED DISRUPTIONS 1`, the drain to finish, and `never zero`. While the node is cordoned, the five's pods from it are `Pending`: the cordoned node no longer counts, the node left takes one pod of each, and there is no room for a third ([Disruption and spread](#disruption-and-spread)). After `uncordon` they schedule back and nothing is `Pending`. If the drained node held no data pod, NAP may remove it as empty first. Then the `Pending` pods get a new node within a few minutes, and `uncordon` reports the node not found. Run this with the five at their minimums. Under the load test api-gateway scales to 3 or 4, puts two pods on a node, and the drain waits on its budget.

Proven on 25 Nov 2026 under the host rule. Each of the five showed `nodes: 2, zones: 2 (uksouth-1 uksouth-3)`, on a `Standard_D2s_v6` in `uksouth-3` and a `Standard_D2nls_v6` in `uksouth-1`, with nothing `Pending`. The drain of `general-8mlzs` exited 0, and the ready log read `never zero`: each of the five dropped to 1 Ready replica, never 0. While the node was cordoned, one pod of each of the five was `Pending`. The scheduler reported `1 node(s) didn't match pod topology spread constraints`, and NAP `all available instance types exceed limits for nodepool (NodePool=general)`. About a minute after the drain, NAP removed the drained node as empty and started `general-kvhpw`, a `Standard_D2nls_v6` in `uksouth-1`. The five's pods were `Pending` for about 3.5 minutes in all, then ran there, and `uncordon` reported the node not found, as above.

**P6-09 (#70), criterion 12: the HPAs match the ADR, load triggers scale-out, and KEDA scales the worker on queue depth.**

Offline, [Validate](#validate) prints each HPA's minimum, maximum and target. Live:

```bash
CTX=aks-v2-platform
K() { kubectl --context "$CTX" -n apps "$@"; }
K get hpa
# In one terminal: the load test, five minutes of logins and page loads through Traefik.
make loadtest DURATION=5m RATE=40
# In another: watch the gateway and dashboard-api scale out, then back after it ends.
K get hpa api-gateway dashboard-api -w
```

Expect seven HPAs at the ADR's minimums and maximums, plus `keda-hpa-worker`. During the load test, api-gateway's CPU passes 70% and it scales towards 4 replicas, and dashboard-api towards 2. About five minutes after the test ends, both return to their minimums. The backends sit behind the gateway's rate limit of 100 requests a minute per client, so a load test from a few pods can't push them hard. That limit is doing its job. KEDA's half of the criterion is in [keda](../keda/README.md#proving-it).

**P6-10 (#71), criterion 13: a simulated eviction of the spot node causes no failed orders.**

The worker must be on a spot node first. If it is on `general`, cordon the `general` nodes for a moment and roll the worker: the new pod can't fit anywhere, so NAP brings up a node from the higher-weight `spot` pool. The old pod keeps working until the new one is Ready, since `maxUnavailable` is 0. `az vm simulate-eviction` evicts the VM. Azure promises 30 seconds' notice, but in the P6 live run the worker pod got about 1 second of its grace period. NAP's nodes are VMs in the node resource group, named after the node. Run the `az` command only on the spot node.

```bash
CTX=aks-v2-platform
K() { kubectl --context "$CTX" "$@"; }
K get nodes -L karpenter.sh/capacity-type,karpenter.sh/nodepool
K -n apps get pods -o wide -l app.kubernetes.io/name=worker
# Only if no worker runs on a spot node:
K cordon -l karpenter.sh/nodepool=general
K -n apps rollout restart deploy/worker
K -n apps rollout status deploy/worker --timeout=10m
K uncordon -l karpenter.sh/nodepool=general

# Orders flowing, then evict the spot node.
make loadtest DURATION=5m &
NODE=$(K get nodes -l karpenter.sh/capacity-type=spot -o jsonpath='{.items[0].metadata.name}')
NRG=$(tofu -chdir=infra/platform output -raw aks_node_resource_group)
az vm simulate-eviction --subscription "$PLATFORM_SUB" --resource-group "$NRG" --name "$NODE"
K get pods -n apps -o wide -l app.kubernetes.io/name=worker -w
wait
az servicebus queue show --subscription "$PLATFORM_SUB" -g "$(tofu -chdir=infra/platform output -raw resource_group_name)" \
  --namespace-name "$(tofu -chdir=infra/platform output -raw servicebus_namespace_name)" \
  -n order-events --query countDetails
```

Expect every other `apps` pod on an on-demand node. After the eviction, the worker pods on that node go, new ones start on another spot node or on `general`. A message the worker was handling comes back when its lock expires, since the pod had no time to abandon it. The load test ends with `http_req_failed` at 0% and exit 0: no order failed. Once the worker catches up, `activeMessageCount` and `deadLetterMessageCount` are 0. A message held by an evicted worker shows a delivery count of 2 in the worker's log when it comes back.

**P6-11 (#72), criterion 7: one copy of each job per tick, across a rollout, a pod delete and a node drain.**

```bash
CTX=aks-v2-platform
K() { kubectl --context "$CTX" -n apps "$@"; }
K get statefulset scheduler -o jsonpath='{.kind} {.spec.replicas}{"\n"}'
# Count scheduler pods that exist, Terminating included, every second, in the background.
( for i in $(seq 600); do K get pods -l app.kubernetes.io/name=scheduler --no-headers 2>/dev/null | wc -l; sleep 1; done \
  | sort | uniq -c > /tmp/scheduler-pods.log ) &

# 1. A rollout.
K rollout restart statefulset/scheduler
K rollout status statefulset/scheduler --timeout=3m
# 2. A pod delete.
K delete pod scheduler-0
K wait pod/scheduler-0 --for=condition=Ready --timeout=3m
# 3. A drain of its node. The selector leaves postgres-0 and redis-0 where they
# are, since their budgets refuse every eviction.
NODE=$(K get pod scheduler-0 -o jsonpath='{.spec.nodeName}')
kubectl --context $CTX drain "$NODE" --ignore-daemonsets --delete-emptydir-data \
  --pod-selector='app.kubernetes.io/name notin (postgres,redis)' --timeout=5m
K wait pod/scheduler-0 --for=condition=Ready --timeout=5m
kubectl --context $CTX uncordon "$NODE"

wait; cat /tmp/scheduler-pods.log
# One expire_reservations run a minute since the drain.
sleep 300
K logs scheduler-0 --since=5m --timestamps | grep "Running job: expire_reservations" | cut -c1-16 | uniq -c
```

Expect `StatefulSet 1`. The pod count log shows only `0` and `1`, never `2`, even while an old pod is `Terminating`. Each minute since the drain shows one `expire_reservations` run. Each new pod's first run comes at startup, after the old pod stopped.

Proven on 25 Nov 2026 under the host rule. The pod count log read `0` twice and `1` 598 times, never `2`. Each minute from 11:29 to 11:33 showed one `expire_reservations` run. The drain of the scheduler's node exited 0. The five's pods from it were `Pending` while it was cordoned and ran again after `uncordon`. The same pod also ran `abandoned_carts` once every 5 minutes from 11:14:49 to 11:59:49, with no duplicate runs. The 11:59:49 run cancelled 699 orders older than 30 minutes, with one cancel line for each.

The drain skips Postgres and Redis. Their budgets are `maxUnavailable: 0` ([data](../data/README.md#scheduling-off-spot-and-safe-to-move)), so `kubectl drain` retries their evictions until `--timeout` and exits 1, and a plain drain fails whenever the scheduler shares a node with either. `--pod-selector` evicts every other pod and leaves those two running on the cordoned node until `uncordon`. The five multi-replica services' pods from that node stay `Pending` until then too, since with it cordoned the node left takes only one pod of each ([Disruption and spread](#disruption-and-spread)). Run this with the five at their minimums, not within the HPA's 300s scale-down window after criterion 12's load test. A service above its minimum can have two pods on the scheduler's node: the drain evicts one, its replacement is `Pending`, the budget allows no more evictions, and the 5m drain exits 1.

## Validate

Run offline, from the repo root. Each overlay builds with its placeholders, which are valid values for the schema checks:

```bash
TMP=$(mktemp -d)
for svc in api-gateway order-service inventory-service payment-service notification-service \
           shipping-service dashboard-api worker scheduler; do
  kustomize build k8s/apps/$svc/overlays/dev; echo ---
done > "$TMP/dev.yaml"

# Criterion 1: valid manifests. The CRD catalog covers ExternalSecret, ScaledObject and
# TriggerAuthentication, so nothing is skipped.
kubeconform -strict -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
  "$TMP/dev.yaml"

# Criteria 1 and 2: one line per workload. Every column after the name must read
# true 65532 RuntimeDefault true false ALL 40m 3
yq -N -r 'select(.kind == "Deployment" or .kind == "StatefulSet") | .spec.template.spec as $p | $p.containers[] |
  [$p.serviceAccountName, $p.securityContext.runAsNonRoot, $p.securityContext.runAsUser,
   $p.securityContext.seccompProfile.type, .securityContext.readOnlyRootFilesystem,
   .securityContext.allowPrivilegeEscalation, (.securityContext.capabilities.drop | join(",")),
   .resources.requests.cpu, .readinessProbe.timeoutSeconds] | @tsv' "$TMP/dev.yaml" | grep .

# Criterion 10: rollout settings for the eight Deployments. Every row reads
# RollingUpdate 0 1 300 45 5. The scheduler is a StatefulSet and has none of
# these: its grace period and preStop come from the same patch as the rest.
yq -N -r 'select(.kind == "Deployment") | [.metadata.name, .spec.strategy.type,
  .spec.strategy.rollingUpdate.maxUnavailable, .spec.strategy.rollingUpdate.maxSurge,
  .spec.progressDeadlineSeconds, .spec.template.spec.terminationGracePeriodSeconds,
  .spec.template.spec.containers[0].lifecycle.preStop.sleep.seconds] | @tsv' "$TMP/dev.yaml" | grep .

# Criterion 12: name, minimum, maximum, CPU target. Expect api-gateway 2 4 70,
# order, inventory, payment and shipping 2 3 70, notification and dashboard-api
# 1 2 70, then the worker's ScaledObject: 1 5 5.
yq -N -r 'select(.kind == "HorizontalPodAutoscaler") | [.metadata.name, .spec.minReplicas,
  .spec.maxReplicas, .spec.metrics[0].resource.target.averageUtilization] | @tsv' "$TMP/dev.yaml" | grep .
yq -N -r 'select(.kind == "ScaledObject") | [.metadata.name, .spec.minReplicaCount,
  .spec.maxReplicaCount, .spec.triggers[0].metadata.messageCount] | @tsv' "$TMP/dev.yaml" | grep .

# Criterion 11: each workload's zone, then hostname, spread: key, rule, minDomains,
# nodeTaintsPolicy. Every zone entry reads topology.kubernetes.io/zone
# ScheduleAnyway null null. The five multi-replica services' hostname entry reads
# kubernetes.io/hostname DoNotSchedule 2 Honor, the other four's
# kubernetes.io/hostname ScheduleAnyway null null.
yq -N -r 'select(.kind == "Deployment" or .kind == "StatefulSet") | .spec.template.spec.topologySpreadConstraints as $t |
  [.metadata.name, $t[0].topologyKey, $t[0].whenUnsatisfiable, $t[0].minDomains, $t[0].nodeTaintsPolicy,
   $t[1].topologyKey, $t[1].whenUnsatisfiable, $t[1].minDomains, $t[1].nodeTaintsPolicy] | @tsv' "$TMP/dev.yaml" | grep .

# Criterion 2: the gateway trusts X-Forwarded-For only from the pod CIDR.
yq -N -r 'select(.kind == "ConfigMap" and .metadata.name == "api-gateway*") | .data.TRUSTED_PROXIES' "$TMP/dev.yaml" | grep .
rm -rf "$TMP"
```

Expect `Valid: 73, Invalid: 0, Errors: 0, Skipped: 0`, then the rows each comment describes, and `10.244.0.0/16`.
