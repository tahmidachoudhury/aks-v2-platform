# Production AKS Platform: Order Fulfillment Platform with 9 microservices.

**TL;DR:** Service code lives in `app/`, Terraform in `infra/`, manifests in `k8s/`, Argo CD config
in `argocd/` and pipelines in `.github/workflows/`. Pipelines trigger on these paths, so keep work in
the folder it belongs to.

## Layout

| Folder | Holds |
|---|---|
| [`app/`](app/) | The nine Go services under `app/services/`, plus the local docker-compose setup |
| [`infra/`](infra/) | Terraform for Azure: network, AKS, container registry (ACR), Key Vault, Service Bus, state |
| [`k8s/`](k8s/) | Kubernetes manifests, one folder per service plus shared platform pieces |
| [`argocd/`](argocd/) | Argo CD Applications that sync `k8s/` into the cluster |
| [`.github/workflows/`](.github/workflows/) | GitHub Actions: image builds, Terraform, manifest checks |
| [`docs/`](docs/) | Brief, service catalogue, diagrams |

## Naming

The folder names under `app/services/` are the service names everywhere else. The folder in `k8s/`,
the ACR repository and the Argo CD Application use the same name, so workflows can filter on paths:

- `app/services/<service>/**` builds and pushes that service's image only.
- `infra/**` runs Terraform plan, and apply after merge.
- `k8s/**` and `argocd/**` need no deploy pipeline. Argo CD syncs them.
