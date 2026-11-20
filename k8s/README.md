# k8s

Kubernetes manifests for everything that runs in the cluster.

Each service gets a folder named after its folder in `app/services/` (Deployment, Service, config,
NetworkPolicy). Shared platform pieces such as PostgreSQL, Redis, the ingress and storage classes
get their own folders alongside them. A service's ServiceAccount takes the same name as its folder, since
its workload identity's federated credential trusts `system:serviceaccount:apps:<folder name>` (see
[`infra/modules/workload-identity`](../infra/modules/workload-identity/README.md)).

## Morning apply

ArgoCD syncs this folder from P7. Until then, `make cluster` from the repo root applies these folders after the persistent and platform OpenTofu applies, in this order. `make up` runs the platform apply and the kubeconfig step first. Each step is also its own target:

1. [`node-pools`](node-pools/README.md) (`make node-pools`). Nothing outside the system pool schedules without it.
2. [`namespaces`](namespaces/README.md) (`make namespaces`). The Helm installs below go into `platform` and don't create it.
3. [`storage`](storage/README.md) (`make storage`).
4. The Helm installs into `platform`: [`external-secrets`](external-secrets/README.md), [`traefik`](traefik/README.md), [`cert-manager`](cert-manager/README.md), then [`external-dns`](external-dns/README.md), each with `make <folder>`.
5. [`backup`](backup/README.md) (`make backup`), last, after Postgres: the hourly snapshot CronJob in `data`.

Every step is safe to run again. The [Makefile](../Makefile) holds the commands and pins the chart versions.
