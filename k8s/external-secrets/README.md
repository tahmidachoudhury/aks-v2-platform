# external-secrets

**TL;DR:** External Secrets Operator (ESO) 2.15.0, installed with Helm into `platform`, and one ClusterSecretStore, `azure-key-vault`, that reads the Key Vault as `id-external-secrets` through workload identity. No ID or vault name is committed: the client ID goes in with `--set-string` at install time, the tenant comes from the AKS webhook, and the vault URL comes from a gitignored `vault.env`. Install it by hand each morning after the node pools and namespaces. ArgoCD takes it over in P7. Filled by P4-06 (#53).

Why ESO and not the CSI driver: [Secrets delivery](../../docs/architecture-decisions.md#secrets-delivery). Why Helm for it: [Manifest tooling](../../docs/architecture-decisions.md#manifest-tooling).

## Files

| Path | What | Applied by |
| --- | --- | --- |
| `values.yaml` | Helm values for chart `external-secrets` 2.15.0 | `helm upgrade --install` |
| `store/` | The ClusterSecretStore `azure-key-vault` | `kubectl apply -k` |
| `store/vault.env.example` | The shape of `vault.env`, which holds the vault URL and is gitignored | |
| `test/` | `eso-smoke-test`, an ExternalSecret for the acceptance test only. Nothing else includes it | `kubectl apply -k`, then delete |

## How it signs in

The chart's ServiceAccount is `external-secrets` in `platform`, which is the subject the federated credential of `id-external-secrets` trusts ([workload-identity](../../infra/modules/workload-identity/README.md#identities)). The controller pod has the label `azure.workload.identity/use: "true"`, so the AKS workload identity webhook injects `AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and a token file. The webhook and cert-controller pods don't talk to Azure, so they don't get the label.

The store sets `authType: WorkloadIdentity` with no `serviceAccountRef`. ESO signs in with the controller's injected credentials, so:

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

The store's `conditions` allow ExternalSecrets in `apps` and `data` only, the namespaces that hold workloads ([P4-07, #54](https://github.com/tahmidachoudhury/aks-v2-platform/issues/54)). An ExternalSecret anywhere else is refused, so add a new namespace here before it can read the vault.

## Pod Security and scheduling

The chart runs under Pod Security Admission `restricted`, which `platform` enforces. All three Deployments run as UID 1000 with `runAsNonRoot`, drop every capability, block privilege escalation, use the `RuntimeDefault` seccomp profile and have a read-only root filesystem. The only volumes are the webhook's certificate Secret and the projected token volume from the workload identity webhook. Both are allowed.

The system pool is tainted `CriticalAddonsOnly` and the chart sets no tolerations, so ESO runs on a NAP `general` node. Requests are 10m CPU each, 64Mi for the controller and 32Mi for the webhook and cert-controller: 30m CPU in all. There are memory limits but no CPU limits.

## Install each morning

ArgoCD installs this from P7. Until then, after the persistent and platform applies, run `kubectl apply -k k8s/node-pools` and apply the P4-07 namespaces. Then:

```bash
# Values from persistent. Neither is committed.
CLIENT_ID=$(tofu -chdir=infra/persistent output -json workload_identity_client_ids | jq -r '."external-secrets"')
echo "vaultUrl=$(tofu -chdir=infra/persistent output -raw key_vault_uri)" > k8s/external-secrets/store/vault.env

# The operator and its CRDs.
helm upgrade --install external-secrets oci://ghcr.io/external-secrets/charts/external-secrets \
  --version 2.15.0 \
  --namespace platform \
  --values k8s/external-secrets/values.yaml \
  --set-string serviceAccount.annotations."azure\.workload\.identity/client-id"="$CLIENT_ID" \
  --wait

# The store. Needs the CRDs from the step above.
kubectl apply -k k8s/external-secrets/store
kubectl get clustersecretstore azure-key-vault
```

The store should show `STATUS Valid` and `READY True`. `vault.env` only changes if the vault does, so writing it again each morning is harmless.

To upgrade ESO, read the chart's release notes for CRD changes first. Then change the version in the command above and in the header of `values.yaml`.

## Acceptance test

This needs a live cluster. It syncs `jwt-secret` into a Secret in `apps` and compares it with the vault without printing either value:

```bash
kubectl apply -k k8s/external-secrets/test
kubectl -n apps wait externalsecret/eso-smoke-test --for=condition=Ready --timeout=2m

VAULT=$(tofu -chdir=infra/persistent output -raw key_vault_name)
diff <(kubectl -n apps get secret eso-smoke-test -o jsonpath='{.data.value}' | base64 -d | shasum -a 256) \
     <(az keyvault secret show --vault-name "$VAULT" -n jwt-secret --query value -o tsv | tr -d '\n' | shasum -a 256) \
  && echo "match"

kubectl delete -k k8s/external-secrets/test
```

`match` means ESO read the vault as `id-external-secrets` with no secret mounted, which also proves P3-18 (#44) for ESO. The `az` call uses the public endpoint, so run it from an IP the vault firewall allows. If the ExternalSecret isn't ready, run `kubectl -n apps describe externalsecret eso-smoke-test` and check the controller's logs (`kubectl -n platform logs deploy/external-secrets`) for the Key Vault error.

The test uses `jwt-secret` because it is the cheapest secret to rotate if the copy is ever left behind: a new value only signs users out.

## P7

- The chart becomes an ArgoCD Helm source: `oci://ghcr.io/external-secrets/charts/external-secrets` at 2.15.0, with `values.yaml` as its values file. The `argocd` root sets the client ID as a Helm parameter from a data source, so it stays out of Git.
- `store/` becomes a Kustomize source. ArgoCD's checkout has no `vault.env`, so P7 removes the generator and the replacement, and the `argocd` root sets `vaultUrl` with a patch on that Application.
- The ClusterSecretStore must sync after the CRDs. A sync wave on the store Application handles that.

## Validate

```bash
helm template external-secrets oci://ghcr.io/external-secrets/charts/external-secrets \
  --version 2.15.0 --namespace platform --values k8s/external-secrets/values.yaml \
  --set-string serviceAccount.annotations."azure\.workload\.identity/client-id"=00000000-0000-0000-0000-000000000000 \
  | kubeconform -strict -ignore-missing-schemas -summary

# -n keeps a real vault.env if you have one.
cp -n k8s/external-secrets/store/vault.env.example k8s/external-secrets/store/vault.env
kustomize build k8s/external-secrets/store | kubeconform -strict -ignore-missing-schemas -summary
kustomize build k8s/external-secrets/test | kubeconform -strict -ignore-missing-schemas -summary
```

kubeconform has no schema for the CRDs or the ESO resources, so it skips them. To check the ClusterSecretStore and ExternalSecret against their CRDs, extract each CRD's `v1` schema from the rendered chart with `yq` and pass it with `-schema-location`.
