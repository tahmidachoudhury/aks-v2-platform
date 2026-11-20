# k8s

Kubernetes manifests for everything that runs in the cluster.

Each service gets a folder named after its folder in `app/services/` (Deployment, Service, config,
NetworkPolicy). Shared platform pieces such as PostgreSQL, Redis, the ingress and storage classes
get their own folders alongside them. A service's ServiceAccount takes the same name as its folder, since
its workload identity's federated credential trusts `system:serviceaccount:apps:<folder name>` (see
[`infra/modules/workload-identity`](../infra/modules/workload-identity/README.md)).

## Morning apply

ArgoCD syncs this folder from P7. Until then, apply these folders by hand each morning, after the
persistent and platform OpenTofu applies, in this order:

1. [`node-pools`](node-pools/README.md): `kubectl apply -k k8s/node-pools`. Nothing outside the system pool schedules without it.
2. [`namespaces`](namespaces/README.md): `kubectl apply -k k8s/namespaces`. The Helm installs below go into `platform` and don't create it.
3. [`storage`](storage/README.md): `kubectl apply -k k8s/storage`.
4. The Helm installs into `platform`, each with the command in its README:
   [`external-secrets`](external-secrets/README.md), [`traefik`](traefik/README.md),
   [`cert-manager`](cert-manager/README.md), then [`external-dns`](external-dns/README.md).
