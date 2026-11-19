# acr

**TL;DR:** The container registry for the nine service images. Basic SKU, Entra ID access only, no admin user and no private endpoint. Filled by P3-10 (#36) and called from the `persistent` root, so images survive the nightly destroy.

## What it creates

One `azurerm_container_registry`:

- SKU Basic, about $5 a month with 10 GiB of storage included.
- Admin user off. Nothing logs in with a shared username and password.
- Anonymous pull off. Basic does not offer anonymous pull anyway, so this only pins the default.
- `prevent_destroy` set, because deleting the registry deletes every image.

Access is through Entra ID roles only. The AcrPull grant for the kubelet identity (P3-11, #37) depends on the cluster, so it belongs in `platform`, not here. The AcrPush grant for CI comes with P3-21.

## Inputs

| Name | Notes |
| --- | --- |
| `name` | Globally unique, 5 to 50 letters and digits, no hyphens. Check with `az acr check-name -n <name>` |
| `resource_group_name` | |
| `location` | |
| `tags` | |

## Outputs

`id`, `name` and `login_server`.

## Repositories

ACR creates a repository on the first push to it, so OpenTofu manages none of them. There is one per service, named after the folder in `app/services/` and the CI test matrix:

| Repository | Image |
| --- | --- |
| `api-gateway` | `<login_server>/api-gateway:<tag>` |
| `order-service` | `<login_server>/order-service:<tag>` |
| `inventory-service` | `<login_server>/inventory-service:<tag>` |
| `payment-service` | `<login_server>/payment-service:<tag>` |
| `notification-service` | `<login_server>/notification-service:<tag>` |
| `shipping-service` | `<login_server>/shipping-service:<tag>` |
| `worker` | `<login_server>/worker:<tag>` |
| `scheduler` | `<login_server>/scheduler:<tag>` |
| `dashboard-api` | `<login_server>/dashboard-api:<tag>` |

## Known gap

Basic has no private endpoint, so the registry is reachable over the internet. Every pull and push still needs an Entra ID token: AcrPull for the kubelet (P3-11) and AcrPush for CI (P3-21). Private endpoints need Premium, at about $50 a month whether used or not, against a budget of about £100. A production team would use Premium with a private endpoint. See [Container registry](../../../docs/architecture-decisions.md#container-registry).
