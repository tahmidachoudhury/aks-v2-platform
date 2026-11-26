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
- Private endpoints for the Key Vault and the state account's blob service (P3-14), with their records in the zones above. See [Private endpoints](#private-endpoints).
- Network Contributor for the cluster identity on `rg-aks-v2-ingress`, the persistent group that holds Traefik's static IP (P4-03). See [Ingress public IP](#ingress-public-ip).
- A federated credential for each workload identity in persistent, trusting this cluster's OIDC issuer and one ServiceAccount, and the Service Bus Data Sender and Receiver roles on the queue (P3-16, P3-17, P3-18). The subjects and roles are in `local.workload_identities` and `local.servicebus_roles`. See [`../modules/workload-identity`](../modules/workload-identity/README.md).
- Later: the other cluster-bound resources, through the modules in [`../modules`](../modules).

AKS creates its own node resource group. This root doesn't manage it, and it goes when the cluster goes.

`make up` from the repo root runs this apply, fetches an Entra ID kubeconfig with the `kubeconfig_command` output, then applies the argocd root, which installs Argo CD, and Argo CD the rest of the cluster. See [Daily routine](../README.md#daily-routine).

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

Six variables are required:

- `subscription_id` is the subscription to deploy into. The provider uses only this value.
- `servicebus_namespace_name` must be globally unique.
- `operator_object_ids` lists the people who apply by hand, who get AKS RBAC Cluster Admin. Find your ID with `az ad signed-in-user show --query id -o tsv`.
- `acr_name` is the registry in persistent. Copy it from `infra/persistent/terraform.tfvars`.
- `key_vault_name` is the vault in persistent. Copy it from the same file.
- `state_storage_account_name` is the state account. Copy `storage_account_name` from `infra/backend.hcl`.

Copy [`terraform.tfvars.example`](terraform.tfvars.example) to `terraform.tfvars`, which is gitignored, and set them there. Every other variable has a default, including the address ranges, and you can override it in the same file.

The CI infra identity, `id-gh-infra`, also gets AKS RBAC Cluster Admin. Platform reads it by name from `rg-aks-v2-identity` in persistent, so apply persistent first. The admin list is fixed and doesn't follow whoever runs the apply, so a person and CI applying in turn don't swap each other's access.

Platform also reads the workload identities by name from `rg-aks-v2-persistent`. Until persistent creates them, a plan here fails with "User Assigned Identity ... was not found".

## AcrPull for the kubelet

Added in P3-11 (#37). The kubelet pulls images as the cluster's kubelet identity, `aks-v2-platform-agentpool`, which AKS creates in the node resource group. This root grants that identity AcrPull on the registry and nothing else: no push, no delete, and no role on the rest of persistent. Pods need no imagePullSecret, and nothing holds a registry password.

The grant lives here, not in persistent, because AKS deletes the kubelet identity with the cluster and creates a new object ID each morning. A grant in persistent would point at yesterday's identity and leave an orphaned assignment every evening. Here it is created after the cluster and destroyed with it.

A user-assigned kubelet identity kept in persistent with a standing grant would save the daily assignment. It also needs Managed Identity Operator for the control plane identity and a `kubelet_identity` block on the cluster, which is more moving parts.

`principal_type` is set to `ServicePrincipal`. CI's role assignment condition only allows grants to service principals, and it checks the type in the request. The registry sits in `rg-aks-v2-persistent` and AcrPull is in `infra_assignable_roles`, so CI can make this grant once it can apply platform.

A new assignment can take a few minutes to reach the registry. A pull straight after the morning apply may fail with 401 and then succeed on the kubelet's retry.

### Acceptance test

The registry holds no images until CI pushes them, so import a small public image first. Run this from the repo root, with `ACR` set to the registry name and `SUB` to the subscription:

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

## Ingress public IP

Added in P4-03 (#50). Traefik's LoadBalancer Service takes a Standard, zone-redundant static IP, `pip-aks-v2-ingress`, that the persistent root holds in its own group, `rg-aks-v2-ingress`. The Service names both with the `azure-pip-name` and `azure-load-balancer-resource-group` annotations in [`k8s/traefik/values.yaml`](../../k8s/traefik/values.yaml), and the cloud provider puts the IP on a frontend of the cluster's load balancer. Persistent's output `ingress_ip_address` shows it. The names are fixed strings in persistent, in this root and in `values.yaml`, with no variables, so change all three together.

**Why persistent.** ExternalDNS writes the A records into the public zone, and they stay there after the evening destroy, overnight and over weekends. If the IP went with the cluster, Azure would hand the released address to the next customer who asks for one. That customer would receive traffic for our names and could pass an HTTP-01 challenge for them, so Let's Encrypt would issue them a certificate for our hostnames. Keeping the IP means the records only ever point at an address we hold. It costs about $3.65 a month around the clock, against $0.88 for the 176-hour schedule. The address also stays the same, so morning DNS caches are never stale.

It is a static IP rather than one AKS creates for the Service, because AKS deletes its own IP with the Service and the cluster. A named IP is also known from OpenTofu before Traefik is installed.

**The role, in this root.** The cluster identity needs Network Contributor wherever the IP lives. The aks module only grants it on `snet-aks-nodes`, which doesn't cover an IP. The cloud provider reads the IP and also lists every public IP in its group, so a role on the IP alone isn't enough. A role on a group that holds anything else would reach that too, which is why the IP has a group to itself. The identity is new every morning, so the role is granted here, on `data.azurerm_resource_group.ingress`, and goes with the cluster. Network Contributor is on CI's ABAC list and the group is in its `infra_resource_group_ids`, but CI can't apply this root until the [known gap](../modules/github-oidc/README.md#known-gap-the-platform-resource-group) closes, so this grant is applied by hand for now.

**Destroy.** The evening destroy deletes the cluster and its load balancer, which releases the IP from the frontend. Nothing deletes the IP itself, which has `prevent_destroy`. The cloud provider doesn't delete or retag an IP it didn't create, and persistent ignores tag changes on it in case one does.

## Private endpoints

Added in P3-14 (#40). Two private endpoints in `snet-private-endpoints`: `pe-key-vault` (subresource `vault`) for the Key Vault in persistent, and `pe-state-blob` for the blob service of the bootstrap state account. Each writes its A record into the matching private DNS zone, so inside the VNet both resolve to an address in `10.20.4.0/24`. Outside the VNet nothing changes.

The endpoints live in this root because they need the VNet, and they go with it every evening. Overnight both resources are reachable only on their public path:

- Key Vault: the persistent root firewalls it to the operators' public IPs. The cluster doesn't need that path while its endpoint exists, since endpoint traffic isn't subject to the firewall.
- State account: public, with no IP firewall. Shared keys are off, so every request needs an Entra ID token and a Storage Blob Data role. CI runs on GitHub-hosted runners with no fixed IPs, and every root reads state over this path, including this root's destroy and morning apply.

The trade-off, the options and the CI access path are in [Private endpoints](../../docs/architecture-decisions.md#private-endpoints). ACR and Service Bus have no private endpoints: see [Container registry](../../docs/architecture-decisions.md#container-registry) and [Event bus](../../docs/architecture-decisions.md#event-bus).

The vault is read with `data "azurerm_key_vault"`. The state account is read with `azurerm_resources`, as persistent does, so its account keys never reach this root's state.

Both endpoints are approved on create, which needs the private endpoint approval permission on each target. The operators are Owner on the subscription. `id-gh-infra` has Contributor on `rg-aks-v2-persistent`, which covers the vault, but nothing on `rg-aks-v2-tfstate`. When CI starts applying this root, it needs that permission on the state account too.

When first introduced, apply this root before persistent. The persistent apply closes the vault firewall, and External Secrets Operator needs the endpoint in place first.

### State account DNS

While `pe-state-blob` exists, the state account's public name is a CNAME to `<account>.privatelink.blob.core.windows.net`. A public resolver follows that on to the public IP. A DNS server that hosts its own `privatelink.blob.core.windows.net` zone answers from that zone instead. Without a record for our account, it returns NXDOMAIN, and every root fails to load state:

```
Error: error loading state: ... dial tcp: lookup <account>.blob.core.windows.net: no such host
```

It only happens while the cluster is up, so it shows first on the evening destroy. GitHub-hosted runners use public DNS and aren't affected. Nothing in the cluster reads state, so it doesn't matter there either.

The fix is on the machine that runs `tofu`. Send lookups for that one name to a public resolver, and leave the rest of the network's DNS alone. On macOS, a file in `/etc/resolver` named after the host does this:

```bash
# storage_account_name in infra/backend.hcl.
ST=<state account name>
sudo mkdir -p /etc/resolver
echo "nameserver 1.1.1.1" | sudo tee "/etc/resolver/$ST.blob.core.windows.net"
# Ends in a public IP.
dig +short "$ST.blob.core.windows.net"
```

Keep the file. It is harmless when the endpoint doesn't exist, so the morning apply and evening destroy both work. On Linux with systemd-resolved, use a drop-in that routes the name to a public resolver instead.

### Acceptance test

This also covers P3-06 (#32), since the zones only hold records once the endpoints exist. Set `KV` to the vault name, `ST` to the state account and `SUB` to the subscription.

```bash
# Both endpoints approved.
az network private-endpoint list --subscription "$SUB" -g rg-aks-v2-platform \
  --query "[].{name:name, status:privateLinkServiceConnections[0].privateLinkServiceConnectionState.status}" -o table

# Each zone holds an A record in the endpoint subnet (P3-06).
for z in privatelink.vaultcore.azure.net privatelink.blob.core.windows.net; do
  az network private-dns record-set a list --subscription "$SUB" -g rg-aks-v2-platform -z "$z" \
    --query "[].{zone:'$z', name:name, ip:aRecords[0].ipv4Address}" -o table
done

# From a pod: both names resolve privately, and HTTPS goes to the private IP.
kubectl --context aks-v2-platform run pecheck --rm -it --restart=Never --pod-running-timeout=5m \
  --image=curlimages/curl:8.11.1 --command -- sh -c "
    nslookup $KV.vault.azure.net
    nslookup $ST.blob.core.windows.net
    curl -sS -o /dev/null -w '%{remote_ip} %{http_code}\n' https://$KV.vault.azure.net/
    curl -sS -o /dev/null -w '%{remote_ip} %{http_code}\n' https://$ST.blob.core.windows.net/"

# Public access on each resource.
az keyvault show --subscription "$SUB" -n "$KV" \
  --query "{public:properties.publicNetworkAccess, default:properties.networkAcls.defaultAction, bypass:properties.networkAcls.bypass}"
az storage account show --subscription "$SUB" -n "$ST" \
  --query "{public:publicNetworkAccess, sharedKey:allowSharedKeyAccess}"
```

What to expect:

- Both endpoints show `Approved`, and each zone has one A record in `10.20.4.0/24`.
- Each `nslookup` returns a `privatelink` CNAME and an address in `10.20.4.0/24`. From a laptop the same lookups return public addresses.
- Each `curl` prints that private address. Ignore the status code, since the request has no token. A timeout means the endpoint subnet's NSG is blocking 443.
- The vault shows `Enabled`, `Deny` and `None`. The state account shows `Enabled` and `false`.

To see the vault refuse an address outside the list, run `az keyvault secret list --vault-name <vault>` from Azure Cloud Shell or any network not in `key_vault_allowed_ip_ranges`. It fails with `ForbiddenByFirewall`. The same command from the laptop succeeds.
