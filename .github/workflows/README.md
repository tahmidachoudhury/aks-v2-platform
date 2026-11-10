# .github/workflows

GitHub Actions workflows: unit tests for each Go service (`test.yml`), image builds for each
service, OpenTofu plan and apply, and checks on manifests. Workflows authenticate to Azure with
OIDC federated to Entra ID, never stored secrets.

GitHub only runs `.yml` and `.yaml` files here, so Actions ignores this README.
