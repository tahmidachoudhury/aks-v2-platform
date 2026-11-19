# platform

**TL;DR:** Holds the daily stack: network, AKS, Service Bus and the rest. Apply it each morning and destroy it each evening.

## What it holds

- A resource group, `rg-aks-v2-platform`, in UK South.
- The VNet `vnet-aks-v2-platform` with a node subnet and a private endpoint subnet, each with an NSG, through [`../modules/network`](../modules/network/README.md). The address plan, including the pod and service CIDRs for AKS, is in that README.
- Private DNS zones for Key Vault and blob, linked to the VNet, also through the network module. ACR and Service Bus get none, since they have no private endpoints. The [network README](../modules/network/README.md#private-dns-zones) says why.
- The AKS cluster `aks-v2-platform`, through [`../modules/aks`](../modules/aks/README.md). Its node resource group is `rg-aks-v2-platform-nodes`.
- AcrPull for the kubelet identity on the registry in persistent (P3-11), so pods pull images with no imagePullSecret. See [AcrPull for the kubelet](#acrpull-for-the-kubelet).
- The Service Bus namespace and the `order-events` queue, through [`../modules/servicebus`](../modules/servicebus/README.md). Its firewall allows only the cluster's egress IP, so the namespace is created after the cluster.
- The Log Analytics workspace `log-aks-v2-platform`, with a 0.5 GB daily cap and the AKS control plane diagnostic setting, through [`../modules/loganalytics`](../modules/loganalytics/README.md). No Container Insights: New Relic does that job.
- Later: private endpoints, federated credentials and the other cluster-bound resources, through the modules in [`../modules`](../modules).

AKS creates its own node resource group. This root doesn't manage it, and it goes when the cluster goes.

After an apply, run `eval "$(tofu output -raw kubeconfig_command)"` to fetch an Entra ID kubeconfig. Then run `kubectl apply -k ../../k8s/node-pools` so user workloads have somewhere to run.

State lives in the `platform.tfstate` blob in the state account.

## Lifecycle

This root is destroyed every evening and applied every morning, so nothing in it can need to survive the night. Anything that does belongs in [`../persistent`](../persistent/README.md). Apply this root before `argocd`, and destroy `argocd` before this root.

## Init, plan and apply

You need `infra/backend.hcl`, generated from the bootstrap outputs. See [`../bootstrap`](../bootstrap/README.md).

```bash
cd infra/platform
tofu init -backend-config=../backend.hcl
tofu plan -out=platform.tfplan
tofu apply platform.tfplan
```

In the evening, after `argocd` is destroyed:

```bash
tofu destroy
```

Three variables are required:

- `servicebus_namespace_name` must be globally unique.
- `operator_object_ids` lists the people who apply by hand, who get AKS RBAC Cluster Admin. Find your ID with `az ad signed-in-user show --query id -o tsv`.
- `acr_name` is the registry in persistent. Copy it from `infra/persistent/terraform.tfvars`.

Copy [`terraform.tfvars.example`](terraform.tfvars.example) to `terraform.tfvars`, which is gitignored, and set them there. Every other variable has a default, including the address ranges, and you can override it in the same file.

The CI infra identity, `id-gh-infra`, also gets AKS RBAC Cluster Admin. Platform reads it by name from `rg-aks-v2-identity` in persistent, so apply persistent first. The admin list is fixed and doesn't follow whoever runs the apply, so a person and CI applying in turn don't swap each other's access.

## AcrPull for the kubelet

Added in P3-11 (#37). The kubelet pulls images as the cluster's kubelet identity, `aks-v2-platform-agentpool`, which AKS creates in the node resource group. This root grants that identity AcrPull on the registry and nothing else: no push, no delete, and no role on the rest of persistent. Pods need no imagePullSecret, and nothing holds a registry password.

The grant lives here, not in persistent, because AKS deletes the kubelet identity with the cluster and creates a new object ID each morning. A grant in persistent would point at yesterday's identity and leave an orphaned assignment every evening. Here it is created after the cluster and destroyed with it.

A user-assigned kubelet identity kept in persistent with a standing grant would save the daily assignment. It also needs Managed Identity Operator for the control plane identity and a `kubelet_identity` block on the cluster, which is more moving parts.

`principal_type` is set to `ServicePrincipal`. CI's role assignment condition only allows grants to service principals, and it checks the type in the request. The registry sits in `rg-aks-v2-persistent` and AcrPull is in `infra_assignable_roles`, so CI can make this grant once it can apply platform.

A new assignment can take a few minutes to reach the registry. A pull straight after the morning apply may fail with 401 and then succeed on the kubelet's retry.

### Acceptance test

The registry holds no images until CI pushes them, so import a small public image first. With `ACR` set to the registry name and `SUB` to the subscription:

```bash
# The kubelet identity holds AcrPull on the registry and nothing else.
KUBELET=$(tofu -chdir=infra/platform output -raw kubelet_identity_object_id)
az role assignment list --subscription "$SUB" --assignee "$KUBELET" --all \
  --query "[].{role:roleDefinitionName, scope:scope}" -o table

# A test image, then a pod that pulls it with no imagePullSecret.
az acr import --subscription "$SUB" -n "$ACR" \
  --source mcr.microsoft.com/azurelinux/busybox:1.36 --image acrpull-check:1
kubectl --context aks-v2-platform run acrpull-check \
  --image="$ACR.azurecr.io/acrpull-check:1" --restart=Never -- sleep 60
kubectl --context aks-v2-platform get pod acrpull-check \
  -o jsonpath='{.spec.imagePullSecrets}{"\n"}'
kubectl --context aks-v2-platform get sa default \
  -o jsonpath='{.imagePullSecrets}{"\n"}'
kubectl --context aks-v2-platform get events \
  --field-selector involvedObject.name=acrpull-check

# Clean up.
kubectl --context aks-v2-platform delete pod acrpull-check
az acr repository delete --subscription "$SUB" -n "$ACR" --repository acrpull-check --yes
```

The role list shows one row, AcrPull, scoped to the registry. Both `imagePullSecrets` lines print empty. The events end with `Successfully pulled image` and `Started`. The system pool is tainted, so Node Auto Provisioning adds a node for the pod first, which takes a minute or two.

`az aks check-acr -g rg-aks-v2-platform -n aks-v2-platform --acr "$ACR.azurecr.io"` runs a similar check from a pod of its own.
