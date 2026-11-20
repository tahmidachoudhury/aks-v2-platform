# external-secrets

**TL;DR:** External Secrets Operator (ESO) 2.15.0 is installed with Helm into `platform`. One ClusterSecretStore, `azure-key-vault`, reads the Key Vault as `id-external-secrets` through workload identity. No ID or vault name is committed. The client ID goes in with `--set-string` at install time, the AKS webhook supplies the tenant, and a gitignored `vault.env` holds the vault URL. `make external-secrets` installs it each morning after the node pools and namespaces. ArgoCD takes it over in P7. Filled by P4-06 (#53).

Why ESO and not the CSI driver: [Secrets delivery](../../docs/architecture-decisions.md#secrets-delivery). Why Helm for it: [Manifest tooling](../../docs/architecture-decisions.md#manifest-tooling).

## Files

| Path | What | Applied by |
| --- | --- | --- |
| `values.yaml` | Helm values for chart `external-secrets` 2.15.0 | `helm upgrade --install` |
| `store/` | The ClusterSecretStore `azure-key-vault` | `kubectl apply -k` |
| `store/vault.env.example` | The shape of `vault.env`, which holds the vault URL and is gitignored | |
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
| Client ID of `id-external-secrets` | ServiceAccount annotation `azure.workload.identity/client-id` | `--set-string` on the Helm install, from `tofu output workload_identity_client_ids` in persistent |
| Tenant ID | ESO's token request | Injected by the AKS webhook. Not set by us |
| Vault URL | `spec.provider.azurekv.vaultUrl` | `store/vault.env`, written from `tofu output key_vault_uri` in persistent. A kustomize `replacements` copies it into the store. The generated ConfigMap is marked `config.kubernetes.io/local-config`, so it never reaches the cluster |

If `vault.env` is missing, `kubectl apply -k k8s/external-secrets/store` fails with an error naming the file. It never applies a placeholder.

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

ArgoCD installs this from P7. Until then, `make up` or `make cluster` installs it each morning. Before you start:

- The persistent and platform roots are applied. Platform creates the federated credential that trusts `system:serviceaccount:platform:external-secrets`.
- `make node-pools` has run, so ESO has a node to land on.
- `make namespaces` has run, so `platform`, `apps` and `data` exist (P4-07, #54).
- `tofu`, `jq` and `helm` are on your PATH. The Makefile passes the cluster's context itself, so your current context doesn't matter.

From the repo root, run `make external-secrets`. The [Makefile](../../Makefile) pins the chart in `ESO_VERSION`. It runs under `set -euo pipefail`, reads both values and checks them before writing anything. A failed `tofu output`, a missing key or an empty value stops it with a message, so it never writes an empty `vaultUrl` or annotates the ServiceAccount with `null`. Then it writes `vault.env`, installs the chart and applies the store. Check the store with:

```bash
kubectl --context aks-v2-platform get clustersecretstore azure-key-vault
```

The store should show `STATUS Valid` and `READY True`. `vault.env` only changes if the vault does, so writing it again each morning is harmless.

To upgrade ESO, read the chart's release notes for CRD changes first. Then change `ESO_VERSION` in the Makefile and the version in the header of `values.yaml`.

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

## P7

- The chart becomes an ArgoCD Helm source: `oci://ghcr.io/external-secrets/charts/external-secrets` at 2.15.0, with `values.yaml` as its values file. The `argocd` root sets the client ID as a Helm parameter from a data source, so it stays out of Git.
- `store/` becomes a Kustomize source. ArgoCD's checkout has no `vault.env`, so P7 removes the generator and the replacement. The `argocd` root sets `vaultUrl` with a patch on that Application.
- The ClusterSecretStore must sync after the CRDs. A sync wave on the store Application handles that.

## Validate

```bash
helm template external-secrets oci://ghcr.io/external-secrets/charts/external-secrets \
  --version 2.15.0 --namespace platform --values k8s/external-secrets/values.yaml \
  --set-string serviceAccount.annotations."azure\.workload\.identity/client-id"=00000000-0000-0000-0000-000000000000 \
  | kubeconform -strict -ignore-missing-schemas -summary

# Build the store from a temporary copy with the example vault.env, so no
# placeholder vault.env is left in the working tree for a later apply to pick up.
TMP=$(mktemp -d)
cp k8s/external-secrets/store/cluster-secret-store.yaml k8s/external-secrets/store/kustomization.yaml "$TMP"
cp k8s/external-secrets/store/vault.env.example "$TMP/vault.env"
kustomize build "$TMP" | kubeconform -strict -ignore-missing-schemas -summary
rm -rf "$TMP"

kustomize build k8s/external-secrets/test | kubeconform -strict -ignore-missing-schemas -summary
```

kubeconform has no schema for the CRDs or the ESO resources, so it skips them. To check the ClusterSecretStore and ExternalSecret against their CRDs, extract each CRD's `v1` schema from the rendered chart with `yq` and pass it with `-schema-location`.
