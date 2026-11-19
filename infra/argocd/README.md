# argocd

**TL;DR:** Will install ArgoCD into the cluster with Helm. Empty until P7-01 (#73) fills it.

## What it will hold

A Helm release for ArgoCD. It is the only in-cluster component OpenTofu manages, since ArgoCD can't install itself. ArgoCD installs everything else from Git.

There are no resources or provider configuration yet. The Helm provider needs a running cluster, and P7-01 adds that wiring.

State lives in the `argocd.tfstate` blob in the state account.

## Lifecycle

Destroyed and applied again with `platform` every day. Apply it after `platform` in the morning, since it needs the cluster. Destroy it before `platform` in the evening, so the Helm release is removed while the cluster still exists.

## Init

Needs `infra/backend.hcl`, generated from the bootstrap outputs. See [`../bootstrap`](../bootstrap/README.md).

```bash
cd infra/argocd
tofu init -backend-config=../backend.hcl
```
