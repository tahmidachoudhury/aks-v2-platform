# external-dns

**TL;DR:** ExternalDNS 0.23 from the `external-dns` chart pinned at 1.23.0. It watches Ingresses and Services and writes A records into the Azure DNS zone `azure.order-fulfilment.tahmidchoudhury.uk`. It signs in as `id-external-dns` through workload identity, with no secret. A TXT registry marks the records it owns, and it changes nothing else. The client ID and subscription ID are not in Git: the install command reads them from OpenTofu and `$SUB`, and stops if either is missing. Filled by P4-05 (#52).

## Install

You need a platform apply, which creates the federated credential, then the NodePools and the `platform` namespace from [`../namespaces`](../namespaces) (P4-07, #54). Set `SUB` to the subscription ID, then run this from the repo root:

```bash
(
set -euo pipefail
: "${SUB:?Set SUB to the subscription ID first.}"
[[ "$SUB" =~ ^[0-9a-f-]{36}$ ]] || { echo "SUB '$SUB' is not a GUID." >&2; exit 1; }

# Values from persistent. None is committed.
CLIENT_ID=$(tofu -chdir=infra/persistent output -json workload_identity_client_ids | jq -er '."external-dns"') \
  || { echo "No external-dns client ID in the persistent outputs. Apply persistent first." >&2; exit 1; }
ZONE=$(tofu -chdir=infra/persistent output -raw dns_zone_name)
ZONE_RG=$(tofu -chdir=infra/persistent output -raw resource_group_name)
[[ "$CLIENT_ID" =~ ^[0-9a-f-]{36}$ ]] || { echo "Client ID '$CLIENT_ID' is not a GUID." >&2; exit 1; }
[[ -n "$ZONE" && -n "$ZONE_RG" ]] || { echo "No dns_zone_name or resource_group_name output." >&2; exit 1; }

helm upgrade --install external-dns external-dns \
  --kube-context aks-v2-platform \
  --repo https://kubernetes-sigs.github.io/external-dns/ --version 1.23.0 \
  --namespace platform -f k8s/external-dns/values.yaml \
  --set-string serviceAccount.annotations."azure\.workload\.identity/client-id"="$CLIENT_ID" \
  --set-string extraArgs.azure-subscription-id="$SUB" \
  --set-string extraArgs.azure-resource-group="$ZONE_RG" \
  --set-string "domainFilters[0]=$ZONE" \
  --wait
)
```

The block runs in a subshell with `set -euo pipefail`, the same pattern as the [External Secrets install](../external-secrets/README.md). A failed `tofu output`, a missing key or a value that isn't a GUID stops it before Helm runs, so the ServiceAccount is never annotated with `null`.

For P7, the argocd root can read `id-external-dns` with a `data "azurerm_user_assigned_identity"` and pass the same four values as Helm parameters on the Application. See [Client IDs](../../infra/modules/workload-identity/README.md#client-ids).

## Settings

| Setting | Value | Why |
| --- | --- | --- |
| `provider` | `azure` | Public Azure DNS, not Azure Private DNS |
| `domainFilters` | The zone, from `dns_zone_name` | The delegated zone. A host outside it is ignored |
| `--azure-resource-group` | The zone's group, from `resource_group_name` | `id-external-dns` has Reader there to list zones, and DNS Zone Contributor on the zone |
| `registry` | `txt` | Each record gets a TXT record naming its owner. ExternalDNS only changes records with its own owner ID |
| `txtOwnerId` | `aks-v2-platform` | Fixed, so each morning's new cluster owns the records the old one wrote |
| `policy` | `sync` | Creates, updates and deletes. A record goes when its Ingress goes. `upsert-only` would leave stale records |
| `sources` | `service`, `ingress` | The chart default. A Service needs an `external-dns.alpha.kubernetes.io/hostname` annotation to get a record. Traefik's Service has none |

The zone and its group come from the persistent outputs rather than `values.yaml`, so their names live only in persistent's variables.

ExternalDNS reads the target IP from the Ingress status. Traefik writes its Service's IP there (`publishedService`, on by default), so every Traefik Ingress points at `pip-aks-v2-ingress`.

The records stay in the zone overnight, after the cluster is deleted. That is safe because the IP they point at stays too: `pip-aks-v2-ingress` lives in persistent, so nobody else can be given the address and answer for our names. See [Ingress public IP](../../infra/platform/README.md#ingress-public-ip).

## Workload identity

The identity, its roles and the federated credential already exist (P3-18, #44): `id-external-dns` in persistent, and a credential in platform that trusts `system:serviceaccount:platform:external-dns`. The chart's default ServiceAccount name is `external-dns` and it installs into `platform`, so both match.

The pod has the label `azure.workload.identity/use: "true"`. The AKS webhook then mounts a projected token and sets `AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and `AZURE_FEDERATED_TOKEN_FILE` from the ServiceAccount annotation.

The Azure provider won't start without a config file at `/etc/kubernetes/azure.json`. Ours holds only `{"useWorkloadIdentityExtension": true}`, set through the chart's `secretConfiguration`. Flags supply the subscription and resource group. The webhook supplies the client ID and tenant.

The chart marks `secretConfiguration` as deprecated. If a later chart drops it, mount the same file from a ConfigMap with `extraVolumes`.

## Client ID and subscription ID

Neither is committed. They are identifiers, not credentials: the client ID only works with a token that the cluster's issuer signs for this ServiceAccount, and the subscription ID grants nothing. But this repo is public, and the GitHub identities' client IDs already stay out of Git. ExternalDNS needs the subscription ID in any case, so one injection path carries both. The [workload-identity README](../../infra/modules/workload-identity/README.md#client-ids) records the decision for every component.

## Pod security

The chart defaults meet Pod Security `restricted`: non-root, no privilege escalation, all capabilities dropped, read-only root file system, `RuntimeDefault` seccomp. Requests are 10m CPU and 32Mi. The limit is 128Mi.

## Acceptance test

The full test, with Traefik and cert-manager, is in the [cert-manager README](../cert-manager/README.md#acceptance-test). The ExternalDNS part on its own, with `SUB` set:

```bash
HOST=test.azure.order-fulfilment.tahmidchoudhury.uk
IP=$(tofu -chdir=infra/persistent output -raw ingress_ip_address)
NS=$(tofu -chdir=infra/persistent output -json dns_zone_name_servers | jq -er '.[0]')

# It signed in through workload identity and found the zone.
kubectl --context aks-v2-platform -n platform logs deploy/external-dns | head -20

# An Ingress with a host in the zone.
kubectl --context aks-v2-platform create deployment whoami --image=traefik/whoami:v1.12.0
kubectl --context aks-v2-platform expose deployment whoami --port=80
kubectl --context aks-v2-platform create ingress whoami --class=traefik --rule="$HOST/*=whoami:80"

# Within a minute or two, the A record points at Traefik's IP.
dig +short "$HOST" "@$NS"
echo "expected $IP"
az network dns record-set list --subscription "$SUB" -g rg-aks-v2-persistent \
  -z azure.order-fulfilment.tahmidchoudhury.uk --query "[].{name:name, type:type}" -o table

# Clean up. ExternalDNS deletes the A and TXT records within a minute.
kubectl --context aks-v2-platform delete ingress,service,deployment whoami
```

The logs show `Using workload identity extension` and no authorisation errors. `dig` returns the ingress IP. The record list shows an A record for `test` and the TXT records that mark it as owned. The test asks one of the zone's own name servers, to avoid a cached negative answer from your resolver.

## Validate

```bash
helm template external-dns external-dns --kube-context aks-v2-platform --repo https://kubernetes-sigs.github.io/external-dns/ --version 1.23.0 \
  -n platform -f k8s/external-dns/values.yaml \
  --set-string 'serviceAccount.annotations.azure\.workload\.identity/client-id=00000000-0000-0000-0000-000000000000' \
  --set-string 'extraArgs.azure-subscription-id=00000000-0000-0000-0000-000000000000' \
  --set-string 'extraArgs.azure-resource-group=rg-aks-v2-persistent' \
  --set-string 'domainFilters[0]=azure.order-fulfilment.tahmidchoudhury.uk' \
  | kubeconform -strict -ignore-missing-schemas -summary
```
