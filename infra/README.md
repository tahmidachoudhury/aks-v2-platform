# infra

**TL;DR:** OpenTofu for the Azure side of the platform, split into four roots by lifecycle. Apply them in order: bootstrap, persistent, platform, argocd. Destroy platform and argocd every evening, and never destroy the other two.

## Roots

Each root has its own folder and its own state, so you plan and apply each one separately. The split follows lifecycle, not resource type: what must survive a teardown sits apart from what is rebuilt every day.

| Root | State | Lifecycle | Holds |
| --- | --- | --- | --- |
| [`bootstrap`](bootstrap/README.md) | Local `terraform.tfstate`, gitignored | Applied once, never destroyed | The state storage account and the cost budget |
| [`persistent`](persistent/README.md) | `persistent.tfstate` | Never destroyed | DNS zone, Key Vault, ACR, and identities referenced from Git |
| [`platform`](platform/README.md) | `platform.tfstate` | Destroyed every evening, applied every morning | Network, private DNS zones, AKS, Service Bus, private endpoints, Log Analytics, federated credentials, Service Bus role assignments |
| [`argocd`](argocd/README.md) | `argocd.tfstate` | Destroyed and applied with platform | The ArgoCD Helm release, the only in-cluster component OpenTofu manages |

Each remote root names only its state blob in its `backend "azurerm"` block. The shared settings come from `infra/backend.hcl`. Generate it from the bootstrap outputs (see [`backend.hcl.example`](backend.hcl.example)):

```bash
tofu init -backend-config=../backend.hcl
```

Reusable pieces go in [`modules/`](modules): network, aks, acr, servicebus, keyvault, dns, loganalytics, github-oidc and workload-identity. Each module folder has a README naming the card that fills it and the root that calls it.

## Apply order

1. `bootstrap`, once, by hand. Then write `infra/backend.hcl`.
2. `persistent`.
3. `platform`.
4. `argocd`, which needs the cluster from platform.

## Daily routine

The daily stack runs from 9am to 5pm to keep within the budget.

Morning, from the repo root:

```bash
make up    # platform apply, kubeconfig, then every in-cluster component
```

Evening:

```bash
make down  # platform destroy
```

`make help` lists every target. `tofu apply` and `tofu destroy` stay interactive, so each shows its plan and waits for `yes`. Run `make init` once per clone, and again after a module is added. The [Makefile](../Makefile) runs the roots in the order above and the in-cluster components in the order [`k8s/README.md`](../k8s/README.md) gives. When `argocd` gets a provider in P7, add it to `up` after platform and to `down` before it.

Once that root exists, argocd is destroyed first, so its Helm release is removed while the cluster still exists. If a root fails with `lookup <account>.blob.core.windows.net: no such host` while the cluster is up, your DNS server hosts its own privatelink zone: see [State account DNS](platform/README.md#state-account-dns). Never run `tofu destroy` in `persistent` or `bootstrap`. Both carry `prevent_destroy` on the resources that matter.

## What goes in persistent

A resource goes in `persistent` when recreating it every day would break something. A DNS zone would get new name servers. A Key Vault name stays reserved after a delete. A registry would lose its images. A released ingress IP could go to someone else while our DNS records still point at it.

Any managed identity whose client ID goes on a ServiceAccount also belongs in `persistent`: ExternalDNS, External Secrets, and the per-service workload identities from P3-16 and P3-18. A recreated identity gets a new client ID, and every install that set the old one breaks. The IDs are injected at install time, not committed: see [Client IDs](modules/workload-identity/README.md#client-ids). Federated credentials depend on the cluster's OIDC issuer, which changes with each new cluster, so they stay in `platform`.

## How platform reads persistent

Platform looks up persistent resources with `data` sources by name, for example `data "azurerm_key_vault"` with the vault name and resource group passed in as variables. It doesn't use `terraform_remote_state`, for these reasons:

- Platform needs read access to the resources, not to persistent's whole state file, which can hold secrets.
- Neither root depends on the other's output names, so persistent can be refactored without breaking platform.
- Variables with defaults already fix the names, so there is nothing to look up in state.

If you change a name in persistent, change it in platform's variables too. A plan in platform fails straight away if they drift.

## Tags

Every resource carries the tags `project`, `managed-by`, `root`, `env` and `owner`, so Cost Management can group spend by project, root and owner. The azurerm provider has no provider-level default tags, so each root builds them in `local.tags` and sets `tags = local.tags` on every resource that takes tags.

## Checks

```bash
tofu fmt -recursive -check infra
cd infra/<root> && tofu init -backend=false && tofu validate
```

Lock files are committed for darwin_arm64 and linux_amd64. After changing a provider pin, run:

```bash
tofu providers lock -platform=darwin_arm64 -platform=linux_amd64
```
