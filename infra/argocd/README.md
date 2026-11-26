# argocd

**TL;DR:** Installs Argo CD into the cluster with Helm, then creates the root Application that points it at [`argocd/apps`](../../argocd/README.md). Argo CD installs everything else from Git. The root carries the values that stay out of the public repo, read here by name from Azure. Applied after `platform` every morning and destroyed before it every evening. Filled by P7-01 (#73) and P7-02 (#74).

## What it holds

| Resource | What |
| --- | --- |
| `kubernetes_namespace_v1.argocd` | The `argocd` namespace, Pod Security `restricted` like `apps`, `data` and `platform` |
| `helm_release.argocd` | Chart `argo-cd` 10.17.1 (Argo CD v3.6.1) from `https://argoproj.github.io/argo-helm`, with [`argo-cd-values.yaml`](argo-cd-values.yaml) |
| `helm_release.root` | Chart `argocd-apps` 2.0.8 from the same repo. It holds one Application, `root`, which renders `argocd/apps` at `target_revision` |

The root is a release of its own because Helm resolves every kind in a release before it installs any of them. The Application kind doesn't exist until the argo-cd release has installed its CRD.

Argo CD 3.6 is the first release tested against Kubernetes 1.37, the cluster's version, per the tested versions table in Argo CD's installation docs. Chart 10.17.1 was published on 9 November 2026.

State lives in the `argocd.tfstate` blob in the state account. It holds the client IDs and the subscription ID, like the persistent state.

## Argo CD's settings

From `argo-cd-values.yaml`:

- Tolerates `CriticalAddonsOnly`, so Argo CD starts on the system pool. NAP has no NodePools until Argo CD applies `k8s/node-pools`.
- No Ingress. Use `kubectl port-forward` to reach the UI ([The UI](../../argocd/README.md#the-ui)).
- Admin with the generated password. Each install writes a new one to `argocd-initial-admin-secret`, so the nightly rebuild rotates it. dex is off since there is no SSO.
- Notifications and the ApplicationSet controller are off. The chart has no switch for the controller, so it runs no replicas.
- Application health, which Argo CD dropped in 1.8, is back in `argocd-cm`, so the root's sync waves wait for each wave. A child passes only once its last sync has Succeeded and it is Healthy, so a hook Job still running holds the wave. One whose sync failed reports Degraded, which fails the root's wave ([How the cluster comes up](../../argocd/README.md#how-the-cluster-comes-up)).
- `crds.keep` is set, so uninstalling the release leaves the Application CRD and doesn't delete every Application.
- Requests are 180m CPU and 480Mi across the controller, repo server, API server and Redis. No CPU limits, as elsewhere.

`main.tf` adds the repository as a connected repo, so the UI lists it. It is public, so there is no credential.

Every pod meets `restricted`: non-root, the RuntimeDefault seccomp profile, no privilege escalation, every capability dropped and no host paths. See [Validate](#validate).

## Signing in to the cluster

The cluster has local accounts disabled. The kubernetes and helm providers use `exec` with `kubelogin get-token --login azurecli`, so they sign in as your `az login`. The `azurerm_kubernetes_cluster` data source gives the API server address and CA, with no credential. This root pins azurerm `~> 5.15`: 4.81's data source panics on a cluster with Node Auto Provisioning ([terraform-provider-azurerm#33604](https://github.com/hashicorp/terraform-provider-azurerm/issues/33604)). It only reads with azurerm, so persistent and platform stay on 4.81 for now. You need `kubelogin` on your PATH and AKS RBAC Cluster Admin, which platform grants to `operator_object_ids`.

## Values from Azure

The root Application gets these as Helm values under `azure`. Each is read by name with a data source, as platform reads persistent ([How platform reads persistent](../README.md#how-platform-reads-persistent)):

| Value | Data source | Name from |
| --- | --- | --- |
| Registry login server | `azurerm_container_registry` | `acr_name` |
| Key Vault URI | `azurerm_key_vault` | `key_vault_name` |
| DNS zone and its resource group | `azurerm_dns_zone` | `dns_zone_name`, `persistent_resource_group_name` |
| Service Bus namespace, FQDN and queue | `azurerm_servicebus_namespace`, `azurerm_servicebus_queue` | `servicebus_namespace_name`, `servicebus_queue_name` |
| Client ID of each workload identity | `azurerm_user_assigned_identity`, one per `id-<name>` | The seven names in `main.tf`, as in persistent |
| Subscription ID, for ExternalDNS | None | `subscription_id` |

A plan fails straight away if a name has drifted from the other roots. `subscription_id` is the platform root's: platform reads the persistent resources in that subscription, and so does this root. [argocd/README.md](../../argocd/README.md#values-that-stay-out-of-git) shows where each value ends up.

## Variables

Copy `terraform.tfvars.example` to `terraform.tfvars`, which is gitignored. `subscription_id`, `acr_name`, `key_vault_name` and `servicebus_namespace_name` are required, with the same values as in the other roots' tfvars. The rest have defaults that match theirs.

`target_revision`, `main` by default, is the branch, tag or commit the root and every child track. To try a branch before merging, set it, then apply again without it. After each apply that changes it on a live cluster, end the root's running operation, and any child's still running on the old revision, or they stay on it ([Testing a branch](../../argocd/README.md#testing-a-branch)).

## Lifecycle

Destroyed and applied again with `platform` every day. `make up` applies it after platform and the kubeconfig step. `make down` destroys it before platform, so the Helm releases go while the cluster still exists. No Application has a finalizer, so the destroy never waits on a cascade ([The evening destroy](../../argocd/README.md#the-evening-destroy)). If it fails, `make down` stops before platform and prints the recovery.

The root retries a failed sync without a limit, every 3 minutes at most, and picks up a new commit on the same branch as it lands. Auto-sync never retries a failed revision by itself, so a root that gave up would leave the later waves undeployed until the next commit.

A plan or apply needs the cluster up, since the providers read it.

## Helm repositories

The helm provider uses a repository config and cache of its own, in the repo's gitignored `.helm/`, resolved from this folder so `make` and plain `tofu` agree. A stale entry in your own Helm config, such as a repo with no cached index, would otherwise fail the install before anything is applied. The Validate commands below use the same path.

## Init

Needs `infra/backend.hcl`, generated from the bootstrap outputs. See [`../bootstrap`](../bootstrap/README.md).

```bash
cd infra/argocd
tofu init -backend-config=../backend.hcl
```

`make init` does this for all three remote roots.

## Validate

Offline, from the repo root. `TF_DATA_DIR` keeps the providers out of the working tree:

```bash
TF_DATA_DIR=$(mktemp -d) sh -c 'tofu -chdir=infra/argocd init -backend=false -input=false >/dev/null && tofu -chdir=infra/argocd validate'
tofu fmt -check infra/argocd

# The chart renders with these values, with the toleration, and with no Ingress.
# The repo's own Helm repository config, as infra/argocd uses, so a stale entry
# in your own Helm config can't fail the render. Run from the repo root.
export HELM_REPOSITORY_CONFIG="$PWD/.helm/repositories.yaml" HELM_REPOSITORY_CACHE="$PWD/.helm/cache"
helm template argocd argo-cd --repo https://argoproj.github.io/argo-helm --version 10.17.1 \
  -n argocd -f infra/argocd/argo-cd-values.yaml > /tmp/argocd.yaml
grep -c 'kind: Ingress' /tmp/argocd.yaml
yq -N -r 'select(.kind == "Deployment" or .kind == "StatefulSet" or .kind == "Job") |
  [.kind + "/" + .metadata.name, (.spec.template.spec.tolerations[0].key)] | @tsv' /tmp/argocd.yaml

# restricted Pod Security: one row per container, every column after the names true or RuntimeDefault.
yq -N -r 'select(.kind == "Deployment" or .kind == "StatefulSet" or .kind == "Job") |
  .spec.template.spec as $p | .metadata.name as $n | (($p.containers // []) + ($p.initContainers // []))[] |
  [$n, .name, (.securityContext.runAsNonRoot // $p.securityContext.runAsNonRoot),
   (.securityContext.seccompProfile.type // $p.securityContext.seccompProfile.type),
   (.securityContext.allowPrivilegeEscalation == false),
   ((.securityContext.capabilities.drop // []) | contains(["ALL"]))] | @tsv' /tmp/argocd.yaml
```

Expect `Success! The configuration is valid.`, no `fmt` output, `0` Ingresses, `CriticalAddonsOnly` on all six workloads, and every container row `true RuntimeDefault true true`.
