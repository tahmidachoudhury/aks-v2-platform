# external-secrets

**TL;DR:** External Secrets Operator (ESO) 2.15.0 runs in `platform`, installed by Argo CD from its Helm chart. One ClusterSecretStore, `azure-key-vault`, reads the Key Vault as `id-external-secrets` through workload identity. No ID or vault name is committed. Argo CD sets the client ID and the vault URL from OpenTofu, and the AKS webhook supplies the tenant. The chart syncs at wave 2 and the store at wave 3, after the node pools and namespaces. Filled by P4-06 (#53), moved under Argo CD by P7-03 (#75).

Why ESO and not the CSI driver: [Secrets delivery](../../docs/architecture-decisions.md#secrets-delivery). Why Helm for it: [Manifest tooling](../../docs/architecture-decisions.md#manifest-tooling).

## Files

| Path | What | Applied by |
| --- | --- | --- |
| `values.yaml` | Helm values for chart `external-secrets` 2.15.0 | The `external-secrets` Application |
| `store/` | The ClusterSecretStore `azure-key-vault`, with a placeholder `vaultUrl` | The `external-secrets-store` Application |
| `test/` | `eso-smoke-test`, an ExternalSecret for the acceptance test only. Nothing else includes it | `kubectl apply -k`, then delete |

## How it signs in

The chart's ServiceAccount is `external-secrets` in `platform`. This is the subject the federated credential of `id-external-secrets` trusts ([workload-identity](../../infra/modules/workload-identity/README.md#identities)). The controller pod has the label `azure.workload.identity/use: "true"`, so the AKS workload identity webhook injects `AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and a token file. The webhook and cert-controller pods don't talk to Azure, so they don't get the label.

The store sets `authType: WorkloadIdentity` with no `serviceAccountRef`, so ESO uses the controller's injected credentials. This means:

- No tenant ID anywhere. The AKS webhook injects it.
- No client ID in the store, only on the ServiceAccount.
- ESO doesn't need permission to mint tokens for other ServiceAccounts, which the `serviceAccountRef` mode needs.

The vault is reached through its private endpoint. From inside the VNet its name resolves to 10.20.4.4, so the vault firewall, which lets in only the operators, doesn't apply.

`id-external-secrets` holds Key Vault Secrets User on the vault and nothing else, so it can read secret values but can't change them.

## IDs and the public repo

Nothing deployment-specific is committed, following [Client IDs](../../infra/modules/workload-identity/README.md#client-ids):

| Value | Needed by | How it gets there |
| --- | --- | --- |
| Client ID of `id-external-secrets` | ServiceAccount annotation `azure.workload.identity/client-id` | Helm `valuesObject` on the `external-secrets` Application, from the identity's data source in `infra/argocd` |
| Tenant ID | ESO's token request | Injected by the AKS webhook. Not set by us |
| Vault URL | `spec.provider.azurekv.vaultUrl` | A kustomize patch on the `external-secrets-store` Application, from the vault's data source in `infra/argocd` |

The root's chart refuses to render without a vault URI shaped like `https://<name>.vault.azure.net/`, so Argo CD never applies the placeholder. Don't apply `store/` by hand: it would apply `VAULT_URL_FROM_ARGOCD`.

## Which namespaces can use it

The store's `conditions` allow ExternalSecrets in `apps` and `data` only, the namespaces that hold workloads ([P4-07, #54](https://github.com/tahmidachoudhury/aks-v2-platform/issues/54)). The store refuses an ExternalSecret anywhere else. Add a new namespace to the `conditions` before it can read the vault.

## Pod Security and scheduling

The chart runs under Pod Security Admission `restricted`, which `platform` enforces. All three Deployments run as UID 1000 with `runAsNonRoot`, drop every capability, block privilege escalation, use the `RuntimeDefault` seccomp profile and have a read-only root filesystem. The only volumes are the webhook's certificate Secret and the projected token volume from the workload identity webhook. Both are allowed.

The system pool is tainted `CriticalAddonsOnly` and the chart sets no tolerations, so ESO runs on a NAP `general` node.

The webhook runs two replicas with a PodDisruptionBudget of `minAvailable: 1`. Its `failurePolicy` is `Fail`, so with no replica up the API server rejects every ExternalSecret and store write. NAP repacks nodes a minute after they go underused, and the budget keeps one replica serving while a node drains. The two replicas may share a node, since spreading them would force a second node for 10m of CPU.

| Pod | Replicas | CPU request | Memory request | Memory limit |
| --- | --- | --- | --- | --- |
| Controller | 1 | 10m | 64Mi | 256Mi |
| Webhook | 2 | 10m each | 32Mi each | 128Mi each |
| Cert-controller | 1 | 10m | 32Mi | 128Mi |
| **Total** | **4 pods** | **40m** | **160Mi** | **640Mi** |

This fits the `platform` quota from P4-07 (500m CPU and 2Gi of requests, 4Gi of memory limits, 20 pods) with room for other components. Every container sets its own requests and memory limit, so the LimitRange defaults never apply, and no limit comes near the LimitRange's 1Gi maximum. There are no CPU limits, so a burst of reconciles uses idle CPU instead of being throttled.

## Install each morning

Argo CD installs it ([argocd](../../argocd/README.md#how-the-cluster-comes-up)). The `external-secrets` Application, at wave 2, renders chart 2.15.0 from `ghcr.io/external-secrets/charts` with `values.yaml`, read through a second source with `ref: values`. Its CRDs are too large for client-side apply, so it syncs with server-side apply, like every Application. The `external-secrets-store` Application, at wave 3, applies `store/` once the CRDs and webhook are up. The platform root must be applied first, since it creates the federated credential that trusts `system:serviceaccount:platform:external-secrets`. Check the store with:

```bash
kubectl --context aks-v2-platform get clustersecretstore azure-key-vault
```

The store should show `STATUS Valid` and `READY True`.

To upgrade ESO, read the chart's release notes for CRD changes first. Then change `targetRevision` on the `external-secrets` Application in `argocd/apps/templates/platform.yaml` and the version in the header of `values.yaml`.

## Acceptance test

This needs a live cluster. It syncs `jwt-secret` into a Secret in `apps` and compares the exact bytes with the vault without printing either value:

```bash
kubectl apply -k k8s/external-secrets/test
kubectl -n apps wait externalsecret/eso-smoke-test --for=condition=Ready --timeout=2m

VAULT=$(tofu -chdir=infra/persistent output -raw key_vault_name)
diff <(kubectl -n apps get secret eso-smoke-test -o jsonpath='{.data.value}' | base64 -d | shasum -a 256) \
     <(az keyvault secret show --vault-name "$VAULT" -n jwt-secret -o json | jq -j .value | shasum -a 256) \
  && echo "match"

kubectl delete -k k8s/external-secrets/test
```

`match` means ESO read the vault as `id-external-secrets` with no secret mounted, which also proves P3-18 (#44) for ESO. The `az` call uses the public endpoint, so run it from an IP the vault firewall allows. If the ExternalSecret isn't ready, run `kubectl -n apps describe externalsecret eso-smoke-test`. Then check the controller's logs (`kubectl -n platform logs deploy/external-secrets`) for the Key Vault error.

The test uses `jwt-secret` because it is the cheapest secret to rotate if the copy is left behind. A new value only signs users out.

## Validate

```bash
helm template external-secrets oci://ghcr.io/external-secrets/charts/external-secrets \
  --version 2.15.0 --namespace platform --values k8s/external-secrets/values.yaml \
  --set-string serviceAccount.annotations."azure\.workload\.identity/client-id"=00000000-0000-0000-0000-000000000000 \
  | kubeconform -strict -ignore-missing-schemas -summary

kustomize build k8s/external-secrets/store | kubeconform -strict -ignore-missing-schemas -summary
kustomize build k8s/external-secrets/test | kubeconform -strict -ignore-missing-schemas -summary
```

kubeconform has no schema for the CRDs or the ESO resources, so it skips them. To check the ClusterSecretStore and ExternalSecret against their CRDs, extract each CRD's `v1` schema from the rendered chart with `yq` and pass it with `-schema-location`.
