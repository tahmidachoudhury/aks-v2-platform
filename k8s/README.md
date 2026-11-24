# k8s

Kubernetes manifests for everything that runs in the cluster.

Each service gets a folder under [`apps`](apps/README.md) named after its folder in `app/services/`
(Deployment, Service, config, NetworkPolicy), as Postgres and Redis sit under `data`. Shared
platform pieces such as the ingress and storage classes get their own folders alongside them. A service's ServiceAccount takes the same name as its folder, since
its workload identity's federated credential trusts `system:serviceaccount:apps:<folder name>` (see
[`infra/modules/workload-identity`](../infra/modules/workload-identity/README.md)).

## Morning apply

Argo CD applies these folders. `make up` from the repo root applies platform, fetches a kubeconfig, then applies [`infra/argocd`](../infra/argocd/README.md), which installs Argo CD and the root Application. The root brings up one Application per folder, in sync waves ([argocd](../argocd/README.md#how-the-cluster-comes-up)):

1. [`node-pools`](node-pools/README.md). Nothing outside the system pool schedules without it. Argo CD tolerates the system pool's taint, so it runs first.
2. [`namespaces`](namespaces/README.md) and [`storage`](storage/README.md). The charts install into `platform` and don't create it.
3. [`external-secrets`](external-secrets/README.md), then its ClusterSecretStore.
4. [`traefik`](traefik/README.md), then [`cert-manager`](cert-manager/README.md) with its ClusterIssuers, [`external-dns`](external-dns/README.md), [`reloader`](reloader/README.md) and [`keda`](keda/README.md).
5. [`data`](data/README.md): Postgres and Redis. It needs the ClusterSecretStore and the `premium-zrs` class.
6. [`backup`](backup/README.md): the hourly snapshot CronJob in `data`.
7. [`migrations`](migrations/README.md): the Job that builds the schema in the empty database. If it fails, the services don't start.
8. [`apps`](apps/README.md), last: the nine services, each its own Application. The images must already be in ACR (`make images`, after a service changes).

Each wave waits until the one before is Healthy, so no step needs a hand. Chart versions are pinned in [`argocd/apps`](../argocd/apps), and the values each chart reads stay in its folder here. Values that must stay out of Git, such as client IDs and the vault URI, come from OpenTofu through the root ([Values that stay out of Git](../argocd/README.md#values-that-stay-out-of-git)).

Two drills stay out of Argo CD: `make restore-drill` ([backup](backup/README.md)) and `make loadtest` ([loadtest](loadtest/README.md)).

Postgres and Redis start empty each morning, since `make down` deletes their disks with the cluster (see [data](data/README.md#the-nightly-destroy)).
