# k8s

Kubernetes manifests for everything that runs in the cluster.

Each service gets a folder under [`apps`](apps/README.md) named after its folder in `app/services/`
(Deployment, Service, config, NetworkPolicy), as Postgres and Redis sit under `data`. Shared
platform pieces such as the ingress and storage classes get their own folders alongside them. A service's ServiceAccount takes the same name as its folder, since
its workload identity's federated credential trusts `system:serviceaccount:apps:<folder name>` (see
[`infra/modules/workload-identity`](../infra/modules/workload-identity/README.md)).

## Morning apply

ArgoCD syncs this folder from P7. Until then, `make cluster` from the repo root applies these folders after the persistent and platform OpenTofu applies, in this order. `make up` runs the platform apply and the kubeconfig step first. Each step is also its own target:

1. [`node-pools`](node-pools/README.md) (`make node-pools`). Nothing outside the system pool schedules without it.
2. [`namespaces`](namespaces/README.md) (`make namespaces`). The Helm installs below go into `platform` and don't create it.
3. [`storage`](storage/README.md) (`make storage`).
4. The Helm installs into `platform`: [`external-secrets`](external-secrets/README.md), [`traefik`](traefik/README.md), [`cert-manager`](cert-manager/README.md), [`external-dns`](external-dns/README.md), [`reloader`](reloader/README.md), then [`keda`](keda/README.md), each with `make <folder>`.
5. [`data`](data/README.md) (`make data`): Postgres and Redis. It needs the ClusterSecretStore from `external-secrets` and the `premium-zrs` class from `storage`. It waits until both pods are Ready. Apps that use them go after it.
6. [`backup`](backup/README.md) (`make backup`), after `make data` brings up Postgres: the hourly snapshot CronJob in `data`.
7. [`migrations`](migrations/README.md) (`make migrate`): the Job that builds the schema in the empty database. It fails `make` if a migration fails, so the services never roll out on a broken schema.
8. [`apps`](apps/README.md) (`make apps`), last: the nine services. It fills in the registry, Service Bus and client IDs from the OpenTofu outputs and waits for every rollout. The images must already be in ACR (`make images`, after a service changes).

Every step is safe to run again. The [Makefile](../Makefile) holds the commands and pins the chart versions. Postgres and Redis start empty each morning, since `make down` deletes their disks with the cluster (see [data](data/README.md#the-nightly-destroy)).
