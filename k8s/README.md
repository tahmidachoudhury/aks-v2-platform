# k8s

Kubernetes manifests for everything that runs in the cluster.

Each service gets a folder named after its folder in `app/services/` (Deployment, Service, config,
NetworkPolicy). Shared platform pieces such as PostgreSQL, Redis, the ingress and storage classes
get their own folders alongside them. A service's ServiceAccount takes the same name as its folder, since
its workload identity's federated credential trusts `system:serviceaccount:apps:<folder name>` (see
[`infra/modules/workload-identity`](../infra/modules/workload-identity/README.md)). Argo CD syncs from here, so do not apply anything in this folder
by hand.
