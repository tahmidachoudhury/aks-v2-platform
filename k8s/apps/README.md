# apps

**TL;DR:** The nine services run in the `apps` namespace. Each has a folder here named after its folder in `app/services/`, with a `base` and a `dev` overlay. One shared Kustomize component gives every workload the same pod shape. `dev/` aggregates the nine overlays and fills in values kept out of Git from a gitignored `azure.env`. `make images` builds each service into ACR, tagged with the last commit that touched its folder. `make apps` applies `dev/` after `make migrate` and waits for every rollout. Key Vault supplies secrets through one ExternalSecret per service, and Reloader restarts only the services whose Secret changed. ArgoCD takes over in P7, with one Application per service folder.

Filled by P6-01 (#62), P6-02 (#63), P6-05 (#66), P6-06 (#67) and P6-11 (#72).

## Layout

```text
k8s/apps/
  components/service/       the pod shape every workload shares (a Kustomize Component)
  <service>/base/           ServiceAccount, Deployment (the scheduler: StatefulSet), Service,
                            ExternalSecret, config.env
  <service>/overlays/dev/   image tag and replica count for dev
  dev/                      all nine dev overlays, plus the values from OpenTofu
```

This follows [data](../data/README.md#why-k8sdatacomponent): one folder per namespace, then one per component. The service folder name is also the name of its ServiceAccount, of the federated credential's subject (`system:serviceaccount:apps:<service>`), and of its ArgoCD Application in P7.

The [Manifest tooling](../../docs/architecture-decisions.md#manifest-tooling) ADR warned of nine near-identical bases and chose a Kustomize component. `components/service` holds what the services share. Each base includes it and keeps only its port, config, Secret and identity. The component patches every Deployment and the scheduler's StatefulSet by kind (`Deployment|StatefulSet`, a regular expression) and merges into the container called `app`. It carries only fields both kinds have. It also labels everything `app.kubernetes.io/part-of: order-fulfilment`, on resources and pod templates but never on selectors.

Each file writes its selectors out. Kustomize's `labels` with `includeSelectors` also rewrites `podSelector`s in NetworkPolicies, which would break any rule that names another service.

## The services

| Service | Port | Dev replicas | Identity | Secret keys | Postgres |
| --- | --- | --- | --- | --- | --- |
| api-gateway | 8080 | 2 | none | `JWT_SECRET`; `REDIS_URL` | no |
| order-service | 8081 | 2 | `id-order-service`, Service Bus Data Sender | `PGPASSWORD` | yes |
| inventory-service | 8082 | 2 | none | `PGPASSWORD` | yes |
| payment-service | 8083 | 2 | `id-payment-service`, Service Bus Data Sender | `PGPASSWORD` | yes |
| notification-service | 8084 | 1 | none | `PGPASSWORD` | yes |
| shipping-service | 8085 | 2 | `id-shipping-service`, Service Bus Data Sender | `PGPASSWORD`; `SHIPPING_WEBHOOK_SECRET` | yes |
| dashboard-api | 8086 | 1 | none | `PGPASSWORD` | yes |
| worker | 8090, health only | 1 | `id-worker`, Service Bus Data Receiver | none | no |
| scheduler | 8091, health only | 1 | none | `PGPASSWORD` | yes |

The replica counts are the [Autoscaling](../../docs/architecture-decisions.md#autoscaling) minimums. The worker has no Service, since nothing calls it. The scheduler has a headless one only because a StatefulSet needs a governing Service. Each base's `config.env` holds the non-secret settings. It becomes a ConfigMap with a hash suffix, so an edit rolls the pods. Service URLs use the Service names in `apps`, such as `http://order-service:8081`.

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
| `terminationGracePeriodSeconds` | 40 | Covers the HTTP services' 5s drain and 30s shutdown, and the scheduler's 20s job wait and 5s stop |
| Startup probe | `/livez`, every 5s, 30 tries | The Postgres users ping the database for up to 120s before they listen |
| Liveness probe | `/livez`, every 10s, 3s timeout, 3 tries | Always 200 while the process runs, so a dependency outage never restarts a pod |
| Readiness probe | `/healthz`, every 5s, 3s timeout, 3 tries | 503 on SIGTERM, for Postgres, and for Service Bus on the worker. The Postgres ping has no timeout, so the probe's 3s bounds it |

Every container port is named `http`, so the probes find it.

## Values from OpenTofu

The repo is public, so nothing deployment-specific is committed ([Client IDs](../../infra/modules/workload-identity/README.md#client-ids)). The bases carry placeholders. `dev/` replaces them from the gitignored `dev/azure.env`. The ClusterSecretStore's `vault.env` uses the same pattern ([external-secrets](../external-secrets/README.md#ids-and-the-public-repo)).

| Key in `azure.env` | From | Replaces |
| --- | --- | --- |
| `acrLoginServer` | persistent `acr_login_server` | `ACR_LOGIN_SERVER` at the front of every image |
| `serviceBusNamespace` | platform `servicebus_fqdn` | `SERVICEBUS_NAMESPACE` on order, payment, shipping and worker |
| `serviceBusQueue` | platform `servicebus_queue_name` | `SERVICEBUS_QUEUE` on the same four |
| `order-service`, `payment-service`, `shipping-service`, `worker` | persistent `workload_identity_client_ids` | `azure.workload.identity/client-id` on each ServiceAccount |

`make apps` checks the shape of every value before it writes the file: a GUID for each client ID, `*.azurecr.io`, `*.servicebus.windows.net`. A failed `tofu output`, a missing key or a malformed value stops it, so it never applies a placeholder or a `null`. If `azure.env` is missing, `kustomize build k8s/apps/dev` fails with an error naming the file. `azure.env.example` shows the shape. Don't copy it into place.

The generated `azure` ConfigMap is marked `config.kubernetes.io/local-config`, so it feeds the replacements and never reaches the cluster.

A per-service overlay applied alone still holds the placeholders. Until P7, apply only `dev/`. In P7 the argocd root passes these values to each Application as Kustomize patches, as it does for the store's `vaultUrl`.

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

## Scheduler: one StatefulSet, not CronJobs

P6-11 (#72) asked which fits the code. The answer is a StatefulSet with one replica and no volumes:

- `app/services/scheduler/main.go` runs five jobs on in-process tickers (1, 5, 15, 30 and 60 minutes). Each runs once at startup, then on its tick. It is a long-running process with a health port.
- The jobs take no lock, so two copies would run every job twice. `expire_reservations` lowers `products.reserved` without checking that the reservation is still active, so two runs release the same stock twice ([Autoscaling](../../docs/architecture-decisions.md#autoscaling)).
- A Deployment's rolling update runs the old and new pods together, and the new one runs every job at startup. `strategy: Recreate` fixes rollouts only. On a drain, an eviction or a `kubectl delete pod`, a Deployment starts the new pod while the old one is still in its 40-second shutdown.
- A StatefulSet creates `scheduler-0` again only once the old `scheduler-0` has gone, whatever removed it. Its rolling update, with one replica, deletes the pod and waits for it before it creates the new one. If a node is lost without a clean shutdown, the pod isn't replaced until the node is confirmed gone, which is the trade the jobs need.
- The scheduler keeps no state, so it has no `volumeClaimTemplates`. The headless Service `scheduler` exists only because a StatefulSet requires `serviceName`.
- CronJobs would need a new entry point per job. `concurrencyPolicy: Forbid` also doesn't stop two Jobs overlapping across a CronJob edit. That change isn't worth making for five short jobs.

**A broken scheduler release needs its pod deleted after the revert.** With one replica, a StatefulSet rolls by deleting `scheduler-0` and creating it from the new template. If that pod never turns Ready, say from a bad image, reverting the tag in Git and applying again doesn't replace it, and neither does `kubectl rollout undo`: the controller waits for the broken pod to turn Ready first, and `rollout status` times out. `make apps` handles this. After it applies, if `scheduler-0` is unready, not already terminating, and from a different revision than the StatefulSet's `updateRevision`, it deletes the pod once and the controller recreates it from the current template. It never deletes a Ready pod or one whose revision matches. By hand, after reverting:

```bash
kubectl --context aks-v2-platform -n apps delete pod scheduler-0
kubectl --context aks-v2-platform -n apps rollout status statefulset/scheduler --timeout=5m
```

The cost: no scheduler copy runs while it rolls out or its node drains, and it has no PodDisruptionBudget or autoscaler. A missed tick only delays the next run, since each job works through whatever is due.

## Quotas

### apps

At the dev replica counts, the nine services run 14 pods. 14 x 40m is 560m of the 1,250m `requests.cpu` quota, and 14 x 64Mi is 896Mi of 4Gi. The migration Job adds 10m and 32Mi while it runs. P6-09 adds the autoscalers and the full arithmetic at their maximums.

### platform quota

| Component | Pods | CPU requests | Memory requests | Memory limits |
| --- | --- | --- | --- | --- |
| Traefik | 2 | 100m | 128Mi | 512Mi |
| cert-manager | 3 | 30m | 160Mi | 640Mi |
| External Secrets | 4 | 40m | 160Mi | 640Mi |
| ExternalDNS | 1 | 10m | 32Mi | 128Mi |
| Reloader | 1 | 10m | 32Mi | 128Mi |
| **Total** | **11 of 20** | **190m of 500m** | **512Mi of 2Gi** | **2,048Mi of 4Gi** |

While it runs, cert-manager's startupapicheck Job adds one pod, 50m, 64Mi and a 256Mi limit, from the LimitRange defaults.

## Morning apply

`make cluster` runs these after `make migrate`, so a failed migration stops `make` before any service rolls out:

```bash
make reloader   # before make data, in the first line of make cluster
make apps
```

`make apps` writes `dev/azure.env` and applies `dev/`. It waits up to 2 minutes for every ExternalSecret in `apps`, then up to 10 minutes for each rollout: eight Deployments and `statefulset/scheduler`. Before waiting, it replaces a `scheduler-0` left unready by an earlier broken release (see [Scheduler](#scheduler-one-statefulset-not-cronjobs)). The images must already be in ACR. Run `make images` once after a service changes, not every morning, since ACR outlives the nightly destroy.

## Proving it

These need a live cluster after `make cluster`. Run them from the repo root. The `az` commands name their subscription, so set it first. The registry and the vault are in the persistent root's:

```bash
SUB=$(awk -F'"' '/^[[:space:]]*subscription_id[[:space:]]*=/ {print $2; exit}' infra/persistent/terraform.tfvars)
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

# Each sees its own client ID (no value printed, only a match per service).
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

**P6-11 (#72), criterion 7: one copy of each job per tick, across a rollout, a pod delete and a node drain.**

```bash
CTX=aks-v2-platform
K="kubectl --context $CTX -n apps"
$K get statefulset scheduler -o jsonpath='{.kind} {.spec.replicas}{"\n"}'
# Count scheduler pods that exist, Terminating included, every second, in the background.
( for i in $(seq 600); do $K get pods -l app.kubernetes.io/name=scheduler --no-headers 2>/dev/null | wc -l; sleep 1; done \
  | sort | uniq -c > /tmp/scheduler-pods.log ) &

# 1. A rollout.
$K rollout restart statefulset/scheduler
$K rollout status statefulset/scheduler --timeout=3m
# 2. A pod delete.
$K delete pod scheduler-0
$K wait pod/scheduler-0 --for=condition=Ready --timeout=3m
# 3. A drain of its node. Pick a node without postgres-0 or redis-0, whose budgets block drains.
NODE=$($K get pod scheduler-0 -o jsonpath='{.spec.nodeName}')
kubectl --context $CTX drain "$NODE" --ignore-daemonsets --delete-emptydir-data --timeout=5m
$K wait pod/scheduler-0 --for=condition=Ready --timeout=5m
kubectl --context $CTX uncordon "$NODE"

wait; cat /tmp/scheduler-pods.log
# One expire_reservations run a minute since the drain.
sleep 300
$K logs scheduler-0 --since=5m --timestamps | grep "Running job: expire_reservations" | cut -c1-16 | uniq -c
```

Expect `StatefulSet 1`. The pod count log shows only `0` and `1`, never `2`, even while an old pod is `Terminating`. Each minute since the drain shows one `expire_reservations` run. Each new pod's first run comes at startup, after the old pod stopped.

## Validate

Run offline, from the repo root. The build uses a temporary copy with the example `azure.env`, so no placeholder file stays behind for a later apply:

```bash
TMP=$(mktemp -d)
cp -R k8s/apps "$TMP/"
cp k8s/apps/dev/azure.env.example "$TMP/apps/dev/azure.env"
kustomize build "$TMP/apps/dev" > "$TMP/dev.yaml"

# Criterion 1: valid manifests. The CRD catalog covers ExternalSecret, so nothing is skipped.
kubeconform -strict -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
  "$TMP/dev.yaml"

# Criteria 1 and 2: one line per workload. Every column after the name must read
# true 65532 RuntimeDefault true false ALL 40m 3
yq ea -r '[select(.kind == "Deployment" or .kind == "StatefulSet")] | .[] | .spec.template.spec as $p | $p.containers[] |
  [$p.serviceAccountName, $p.securityContext.runAsNonRoot, $p.securityContext.runAsUser,
   $p.securityContext.seccompProfile.type, .securityContext.readOnlyRootFilesystem,
   .securityContext.allowPrivilegeEscalation, (.securityContext.capabilities.drop | join(",")),
   .resources.requests.cpu, .readinessProbe.timeoutSeconds] | @tsv' "$TMP/dev.yaml"

# Criterion 2: the gateway trusts X-Forwarded-For only from the pod CIDR.
yq ea -r '[select(.kind == "ConfigMap" and .metadata.name == "api-gateway*")] | .[].data.TRUSTED_PROXIES' "$TMP/dev.yaml"
rm -rf "$TMP"
```

Expect `Valid: 45, Invalid: 0, Errors: 0, Skipped: 0`, nine identical rows apart from the name, and `10.244.0.0/16`.
