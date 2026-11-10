# Architecture Decisions

**TL;DR:** All six P0 decisions are settled. We use Service Bus Standard, Kustomize plus Helm, in-cluster Postgres, HPA with KEDA, New Relic and External Secrets Operator. Infrastructure is written in OpenTofu, not Terraform.

Prices are rough monthly figures in US dollars (USD) for the UK South region. They come from the [Azure Retail Prices API](https://prices.azure.com/api/retail/prices) and the vendor pricing pages. A month is 730 hours.

## Summary

| Decision                           | Choice                                                          | Status   | Ticket                                                             |
| ---------------------------------- | --------------------------------------------------------------- | -------- | ------------------------------------------------------------------ |
| [Event bus](#event-bus)            | Azure Service Bus Standard, one queue with a dead-letter queue  | Accepted | [#6](https://github.com/tahmidachoudhury/aks-v2-platform/issues/6)   |
| [Manifest tooling](#manifest-tooling) | Kustomize for our services, Helm for third-party software    | Accepted | [#7](https://github.com/tahmidachoudhury/aks-v2-platform/issues/7)   |
| [Postgres](#postgres)              | StatefulSet in the cluster                                      | Accepted | [#8](https://github.com/tahmidachoudhury/aks-v2-platform/issues/8)   |
| [Autoscaling](#autoscaling)        | HPA for HTTP services, KEDA for the worker, fixed for the rest  | Accepted | [#9](https://github.com/tahmidachoudhury/aks-v2-platform/issues/9)   |
| [Observability](#observability)    | New Relic                                                       | Accepted | [#10](https://github.com/tahmidachoudhury/aks-v2-platform/issues/10) |
| [Secrets delivery](#secrets-delivery) | External Secrets Operator (ESO)                              | Accepted | [#11](https://github.com/tahmidachoudhury/aks-v2-platform/issues/11) |
| [Infrastructure as code](#infrastructure-as-code) | OpenTofu, not Terraform                  | Accepted | none                                                               |

## Event bus

**Status:** Accepted

**Context:** The original authors wrote the services for Amazon SQS. order-service, payment-service and shipping-service publish events and the worker consumes them (see the [service catalogue event flow](service-catalogue.md#event-flow)). The publishers and the worker's receive loop are stubs, so we write the messaging code fresh whichever bus we pick.

**Decision:** Azure Service Bus Standard, one queue with `max_delivery_count` set so failed messages move to the dead-letter queue (DLQ). An IP firewall rule allows only the cluster's outbound IP. Standard costs about $10 a month against $677 for Premium, which a learning build cannot justify. The ticket records the choice but not the reasoning, so the reasons come from the comparison.

**Options compared**

| Option                         | Monthly cost                                                                 | Ops burden                                                   | Dead-letter support                                                                   | Client changes                                      |
| ------------------------------ | ---------------------------------------------------------------------------- | ------------------------------------------------------------ | ------------------------------------------------------------------------------------- | --------------------------------------------------- |
| Service Bus Standard           | About $10 base. The first 13M operations a month are free, then $0.80 per million | None. Azure runs it                                     | Native. A message moves to the DLQ after `max_delivery_count` deliveries (default 10) | `azservicebus` Go SDK in three publishers and worker |
| Service Bus Premium (1 unit)   | About $677                                                                   | None                                                         | Native, as Standard                                                                   | As Standard                                         |
| Event Hubs Standard, Kafka endpoint | About $22 for one throughput unit, plus $0.028 per million events. See the unconfirmed note below | None                                      | None. The consumer writes failures to its own retry or dead-letter topic              | A Kafka Go client, plus our own retry and DLQ logic |
| Strimzi Kafka in the cluster   | No licence. Roughly $90 of capacity: about one `Standard_D2s_v5` node ($81) and three small Premium disks ($3 each). Estimate, not measured | High. We run the operator, three brokers across zones, upgrades and disks | None. Same as Event Hubs | As Event Hubs                                       |

Prices checked on 9 November 2026, UK South. Sources: [Service Bus pricing](https://azure.microsoft.com/en-gb/pricing/details/service-bus/), [Event Hubs pricing](https://azure.microsoft.com/en-gb/pricing/details/event-hubs/), [Linux VM pricing](https://azure.microsoft.com/en-gb/pricing/details/virtual-machines/linux/), [managed disk pricing](https://azure.microsoft.com/en-gb/pricing/details/managed-disks/), and the [Azure Retail Prices API](https://prices.azure.com/api/retail/prices). Unconfirmed: the price API lists a "Standard Kafka Endpoint" meter for Event Hubs at $0.09 an hour (about $66 a month). The pricing page shows no separate Kafka charge.

**Consequences / trade-offs**

- Service Bus gives the dead-letter behaviour the brief asks for with one OpenTofu setting. With Kafka we build it.
- Service Bus private endpoints need the Premium tier ([Private Link docs](https://learn.microsoft.com/en-us/azure/service-bus-messaging/private-link-service)). The brief asks for a private endpoint, and Premium costs about $677 a month.
- Standard can restrict access with IP firewall rules set through OpenTofu, but has no private endpoint. Event Hubs Standard supports Private Link ([Event Hubs quotas](https://learn.microsoft.com/en-us/azure/event-hubs/event-hubs-quotas)).
- Known gap: Service Bus has no private endpoint in this build. P3-14 drops Service Bus and keeps private endpoints for ACR, Key Vault and state storage. A production team should pay for Premium and add the private endpoint.
- Standard caps messages at 256 KB. Our event payloads are small JSON, so this does not matter.
- The code has three publishers, not the four the brief mentions. scheduler only has a comment about a cancellation event.

Ticket: [#6](https://github.com/tahmidachoudhury/aks-v2-platform/issues/6)

## Manifest tooling

**Status:** Accepted

**Context:** We need manifests for nine services we own, plus third-party software: Traefik, cert-manager, ESO and New Relic. Argo CD deploys both Kustomize and Helm sources.

**Decision:** A mix. Kustomize base plus overlays for the nine services and any other manifests we write. Helm for third-party software. Helm suits software you install or distribute. Kustomize suits manifests you own and vary per environment.

**Options compared**

| Option                                 | For                                                                          | Against                                                                       |
| -------------------------------------- | ---------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| Kustomize base plus overlays           | Easy to read. No template logic by design. Overlays patch per environment     | Nine near-identical bases unless we share a Kustomize component               |
| Helm everywhere                        | Templates and values suit many copies that differ a little. Works as a package manager | Go template logic makes manifests hard to read                     |
| One small Helm chart for all nine (alternative considered) | One template, nine values files. Least duplication       | Our manifests pick up template logic, and every service change goes through the shared chart |
| Mix (chosen)                           | Each tool does the job it suits. Vendors ship Helm charts already            | Two tools to learn and two source types in Argo CD                            |

**Consequences / trade-offs**

- Third-party charts install as Helm sources in Argo CD, pinned by chart version.
- Our services stay plain YAML that the README Architecture section can quote as is.
- If the nine bases drift apart, move the shared Deployment, Service and HPA shape into a Kustomize component before using a chart.

Ticket: [#7](https://github.com/tahmidachoudhury/aks-v2-platform/issues/7)

## Postgres

**Status:** Accepted

**Context:** Seven services share one Postgres database, `orders`, as one user. The brief requires Postgres on a StatefulSet with a 20Gi Premium SSD persistent volume claim (PVC).

**Decision:** Run Postgres as a single-replica StatefulSet in the cluster, as the brief requires. This is acceptable for a learning build since the data is disposable and the aim is to operate stateful workloads on Kubernetes. A production team should use Azure Database for PostgreSQL Flexible Server, General Purpose `D2ds_v5`, with zone-redundant high availability (HA).

**Options compared**

| Option                                   | Monthly cost                                                         | HA                                                                         | Backups                                                                                              | Ops burden                                         | Max user connections |
| ---------------------------------------- | -------------------------------------------------------------------- | -------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- | -------------------------------------------------- | -------------------- |
| StatefulSet, 20Gi Premium SSD            | About $6.39 for the disk (billed as a 32 GiB P4, LRS) or $9.58 (ZRS). Compute comes from existing nodes | None. One pod. A restart or reschedule is downtime | Ours to build: VolumeSnapshots (incremental, $0.053 per GB a month) or `pg_dump`. No point-in-time restore | High. We own upgrades, tuning, backups and restores | 100 by default (Postgres `max_connections`) |
| Flexible Server Burstable B1ms           | About $18 ($13.87 compute, $4.26 for the 32 GiB minimum storage)     | Zone-redundant HA not available on Burstable                               | Automatic, 7 days default, up to 35. Point-in-time restore. Backup storage free up to the provisioned size | Low                                                | 35                   |
| Flexible Server Burstable B2s            | About $60 ($55.48 compute, $4.26 storage)                            | As B1ms                                                                    | As B1ms                                                                                              | Low                                                | 414                  |
| Flexible Server General Purpose D2ds_v5  | About $155 ($150.38 compute, $4.26 storage). About $309 with zone-redundant HA, as the standby matches the primary | Zone-redundant HA. Failover in 60 to 120 seconds, no data loss | As B1ms | Low | 844 |

Prices checked on 9 November 2026, UK South. Sources: [Flexible Server pricing](https://azure.microsoft.com/en-gb/pricing/details/postgresql/flexible-server/), [managed disk pricing](https://azure.microsoft.com/en-gb/pricing/details/managed-disks/), and the [Azure Retail Prices API](https://prices.azure.com/api/retail/prices). Behaviour: [Flexible Server HA](https://learn.microsoft.com/en-us/azure/postgresql/high-availability/concepts-high-availability), [backup and restore](https://learn.microsoft.com/en-us/azure/postgresql/backup-restore/concepts-backup-restore), [connection limits](https://learn.microsoft.com/en-us/azure/postgresql/configure-maintain/concepts-limits), [incremental snapshots](https://learn.microsoft.com/en-us/azure/virtual-machines/disks-incremental-snapshots), [Postgres `max_connections`](https://www.postgresql.org/docs/current/runtime-config-connection.html).

**Zone behaviour**

- An LRS disk lives in one zone. AKS cannot attach it to a node in another zone ([AKS availability zones](https://learn.microsoft.com/en-us/azure/aks/availability-zones)). If the Postgres pod moves, it can only land on a node in the disk's zone. With no node there, or that zone down, the pod stays `Pending` until a node appears.
- A ZRS disk replicates across zones and attaches to a node in any zone of the region. The pod can reschedule to another zone and the disk follows, after a detach and attach. ZRS costs about 1.5 times LRS here ($9.58 against $6.39).
- ZRS protects the disk only. A single Postgres pod still means downtime while it moves.

**Consequences / trade-offs**

- Use `Premium_ZRS` for the Postgres and Redis StorageClass. It costs $3.19 a month more for the Postgres disk and removes the zone pinning.
- The connection limit matters more than the price. The services' Go pools allow up to 115 open connections at one replica each. This is above the default 100 in-cluster and far above B1ms's 35. See [Autoscaling](#autoscaling).
- We must build and test the restore procedure ourselves. Flexible Server has point-in-time restore built in.

Ticket: [#8](https://github.com/tahmidachoudhury/aks-v2-platform/issues/8)

## Autoscaling

**Status:** Accepted

**Context:** The brief asks which services scale, on what metric, with the Horizontal Pod Autoscaler (HPA) or Kubernetes Event-driven Autoscaling (KEDA), and what stays fixed. The table comes from the code in `app/services/`.

**Decision**

| Service              | Scaler | Metric                                              | Min | Max | Reason                                                                                                     |
| -------------------- | ------ | --------------------------------------------------- | --- | --- | ---------------------------------------------------------------------------------------------------------- |
| api-gateway          | HPA    | CPU, 70% of request                                 | 2   | 4   | Stateless. Rate-limit counters live in Redis, so replicas share them. Public entry point, so two for zone spread |
| order-service        | HPA    | CPU, 70% of request                                 | 2   | 3   | On the order path. Each pod can open 25 Postgres connections                                               |
| inventory-service    | HPA    | CPU, 70% of request                                 | 2   | 3   | Reservations take row locks, so more pods mostly add lock waits. 25 connections a pod                      |
| payment-service      | HPA    | CPU, 70% of request                                 | 2   | 3   | On the order path. 25 connections a pod                                                                    |
| shipping-service     | HPA    | CPU, 70% of request                                 | 2   | 3   | Takes public carrier webhooks. 15 connections a pod                                                        |
| notification-service | HPA    | CPU, 70% of request                                 | 1   | 2   | Off the synchronous order path. Only the worker will call `/send`. 10 connections a pod                    |
| dashboard-api        | HPA    | CPU, 70% of request                                 | 1   | 2   | Admin traffic. Its queries are aggregates, so load lands on Postgres, not the pod                          |
| worker               | KEDA   | Service Bus active message count, 5 per replica (KEDA default) | 1 | 5 | No database. It handles one message at a time, so replicas add throughput. Queue depth shows the backlog, CPU does not |
| scheduler            | None   | None                                                | 1   | 1   | Jobs run in-process with no lock. Two copies would release the same expired reservation twice              |
| Postgres             | None   | None                                                | 1   | 1   | One primary, no replication. A second replica is a second, empty database                                  |
| Redis                | None   | None                                                | 1   | 1   | Same as Postgres. Rate-limit counters must live in one place                                               |

Behaviour: [KEDA Service Bus scaler](https://keda.sh/docs/latest/scalers/azure-service-bus/).

**Notes on the table**

- notification-service and dashboard-api start at one replica, not two. Neither sits on the path a customer waits on.
- scheduler needs `strategy: Recreate`, not only one replica. A rolling update runs the old and new pod together, and each job runs once at start-up (`runEvery` in `app/services/scheduler/main.go`). `expire_reservations` lowers `products.reserved` without checking the reservation is still active, so two runs can release the same stock twice.
- Postgres connections set the low max replica counts. CPU does not.

**Consequences / trade-offs**

- Postgres connections break first under load. `SetMaxOpenConns` is hard-coded per service: 25 for order, inventory and payment, 15 for shipping, 10 for notification and dashboard, 5 for scheduler. Go opens connections only when needed, but at full load the minimums allow 205 and the maximums 315. Postgres allows 100 by default.
- Options for the limit: raise `max_connections` in the StatefulSet and size its memory to match, add PgBouncer, or make the pool size configurable in code.
- HPA needs CPU requests on every Deployment.
- KEDA has nothing to scale until the worker's Service Bus consumer exists. The receive loop is a stub.
- The worker could scale to zero. One idle pod costs little and avoids a cold start on the first order.

Ticket: [#9](https://github.com/tahmidachoudhury/aks-v2-platform/issues/9)

## Observability

**Status:** Accepted

**Context:** One small AKS cluster runs nine services. The brief asks us to compare New Relic, a self-hosted kube-prometheus-stack (Prometheus, Grafana, Alertmanager), and Azure Managed Prometheus with Azure Managed Grafana. This section feeds P10-07.

**Decision:** New Relic. It is the easiest to set up. It is also the most expensive once you build a team, since you pay per full user. For a real team, pick Azure Managed Prometheus with Grafana. Managed Grafana users cost $6 each a month and metrics stay for 18 months.

**Options compared**

| Option                                       | Monthly cost at this scale                                                                                       | Ingest pricing                                                                                     | Retention                                    | Ops burden                                              | Lock-in                                                     |
| -------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- | -------------------------------------------- | ------------------------------------------------------- | ----------------------------------------------------------- |
| New Relic                                    | $0 for one engineer if ingest stays under 100 GB. A team of five full users on Standard is $406 ($10 for the first, $99 each after) | 100 GB a month free, then $0.40 per GB (original data) or $0.60 (Data Plus). Covers metrics, logs and traces | At least 8 days by default | Low. Install the Helm chart and go | High. New Relic Query Language (NRQL) dashboards and alerts do not move. OpenTelemetry ingest lowers it |
| kube-prometheus-stack, self-hosted           | No licence. Roughly $25 to $45: a share of a node for Prometheus and Grafana plus a $6.39 Premium disk. Estimate | None. You pay in node capacity and disk                                                             | Chart default 10 days, limited by disk size  | High. We run upgrades, storage, HA and alert routing    | Low. Open source, PromQL and Grafana                        |
| Azure Managed Prometheus plus Grafana        | Roughly $28 for metrics, assuming 20,000 series scraped every 30 seconds (estimate). Grafana: $0 for Azure Monitor dashboards with Grafana (preview), or about $62.50 plus $6 per active user for Managed Grafana Standard X1 | $0.16 per 10 million samples ingested, $0.001 per 10 million samples queried | 18 months included | Low. Azure runs storage and scraping | Medium. PromQL and Grafana move, the Azure Monitor workspace and its data rules do not |

Prices checked on 9 November 2026, UK South for Azure. Sources: [New Relic pricing](https://newrelic.com/pricing), [Azure Monitor pricing](https://azure.microsoft.com/en-gb/pricing/details/monitor/), [Managed Grafana pricing](https://azure.microsoft.com/en-gb/pricing/details/managed-grafana/), [managed disk pricing](https://azure.microsoft.com/en-gb/pricing/details/managed-disks/), the [Azure Retail Prices API](https://prices.azure.com/api/retail/prices), [Managed Prometheus overview](https://learn.microsoft.com/en-us/azure/azure-monitor/metrics/prometheus-metrics-overview), [kube-prometheus-stack values](https://github.com/prometheus-community/helm-charts/blob/main/charts/kube-prometheus-stack/values.yaml). Unconfirmed: our real ingest volume and series count. We have not measured either yet.

**Consequences / trade-offs**

- New Relic covers logs. The two Prometheus options do not. With them, container logs go to Log Analytics at $2.88 per GB after the first 5 GB a month. The other choice is Loki, which we would run ourselves.
- Keep log ingest in check from day one. The 100 GB free allowance is why New Relic costs nothing here.
- Instrument the services with OpenTelemetry, not the New Relic agent. This keeps a later move to Prometheus and Grafana cheap.

Ticket: [#10](https://github.com/tahmidachoudhury/aks-v2-platform/issues/10)

## Secrets delivery

**Status:** Accepted

**Context:** Seven services need `DATABASE_URL` and api-gateway needs `JWT_SECRET`. All read secrets from environment variables at start-up. The brief allows External Secrets Operator (ESO) or the Secrets Store CSI Driver (the AKS `azure-keyvault-secrets-provider` add-on) to bring them from Azure Key Vault.

**Decision:** External Secrets Operator. ESO copies Key Vault secrets into Kubernetes Secrets and pods read those, so pods do not depend on Key Vault at start-up. The CSI driver mounts secrets as the pod starts, so a Key Vault or network problem stops pods from starting.

**Options compared**

| Option                          | How it gets secrets                                                                                  | Rotation story                                                                                                                | Pod restart needed?                                                                                       | If Key Vault is down                                                    |
| ------------------------------- | ---------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------- |
| External Secrets Operator       | A controller syncs Key Vault secrets into Kubernetes Secrets                                         | ESO re-reads Key Vault every `refreshInterval` (default 1 hour) and updates the Secret                                        | Yes for environment variables. Use Reloader, or a rollout, to restart the Deployments                     | Existing Secrets stay. Pods keep starting. The ExternalSecret shows not ready |
| Secrets Store CSI Driver add-on | The driver mounts secrets as files when the pod starts. `secretObjects` can also sync them into a Kubernetes Secret | With `--enable-secret-rotation`, the driver polls every 2 minutes and updates the mounted files and the synced Secret | Yes for environment variables (Microsoft suggests Reloader). No if the app re-reads the mounted file | New pods cannot mount the volume and do not start |

Behaviour checked on 9 November 2026. Sources: [CSI driver configuration options](https://learn.microsoft.com/en-us/azure/aks/csi-secrets-store-configuration-options), [ESO ExternalSecret spec](https://external-secrets.io/latest/api/spec/).

**Consequences / trade-offs**

- Our services read secrets from environment variables. CSI would need `secretObjects` anyway, and that synced Secret exists only while a pod mounts the volume. ESO fits the code better.
- Secrets live in etcd as Kubernetes Secrets. CSI without sync keeps them out. Restrict `get secrets` in RBAC.
- Rotating the database password restarts all seven services, since they read `DATABASE_URL` once at start-up. It also needs an `ALTER ROLE` in Postgres.
- To avoid an outage, create a second Postgres role, switch the Secret to it, let Reloader roll the pods, then drop the old role.
- ESO still needs Key Vault for the first sync and for any new secret. It moves the dependency from pod start to sync time.
- ESO is a third-party install, so it goes in with Helm per [Manifest tooling](#manifest-tooling).

Ticket: [#11](https://github.com/tahmidachoudhury/aks-v2-platform/issues/11)

## Infrastructure as code

**Status:** Accepted

**Context:** The brief asks for Terraform with remote state in an Azure Storage Account. In August 2023, HashiCorp moved Terraform 1.6 and later from the Mozilla Public License 2.0 (MPL 2.0) to the Business Source License 1.1 (BSL). The BSL restricts use in competing products. IBM completed its purchase of HashiCorp in February 2025. One company now owns Terraform's licence and roadmap and can change either again.

**Decision:** OpenTofu. It is the open-source fork of Terraform 1.5. It stays on MPL 2.0, which puts no limit on how we use it, and a foundation runs it rather than a single vendor. It reads the same HCL and uses the same `azurerm` provider and `azurerm` state backend with blob lease locking. The rest of the brief's state design holds unchanged. If IBM makes major changes to Terraform's licence, pricing or direction, this project is not affected and has nothing to migrate.

**Options compared**

| Option    | Licence                                                         | Who controls it                                                   | If the owner changes direction                                     |
| --------- | --------------------------------------------------------------- | ----------------------------------------------------------------- | ------------------------------------------------------------------ |
| Terraform | BSL 1.1 since 1.6. Source-available, not open source            | HashiCorp, owned by IBM                                           | We follow their licence and release line, or migrate under pressure |
| OpenTofu  | MPL 2.0, open source (OSI-approved)                             | Linux Foundation project, in the CNCF Sandbox since April 2025    | No single vendor can relicense it. The code can always be forked again |

Checked on 10 November 2026. Sources: [OpenTofu](https://en.wikipedia.org/wiki/OpenTofu), [Terraform and OpenTofu timeline](https://hidekazu-konishi.com/entry/terraform_and_opentofu_history_and_timeline.html), [OpenTofu joins the CNCF](https://thenewstack.io/opentofu-joins-cncf-new-home-for-open-source-iac-project/).

**Consequences / trade-offs**

- The CLI is `tofu`, not `terraform`: `tofu init`, `tofu plan`, `tofu apply`. The infra workflow, when it exists, installs it with `opentofu/setup-opentofu`.
- Code stays HCL in `.tf` files. The working directory and lock file keep their Terraform names (`.terraform/`, `.terraform.lock.hcl`), so the gitignore entries do not change.
- Providers come from the OpenTofu registry, which serves the same providers, including `hashicorp/azurerm`.
- The two tools have drifted apart since 1.6. Where their docs disagree, follow the OpenTofu docs.
- OpenTofu can encrypt state and plan files on the client, which Terraform cannot. Consider it for the state account (P3-01).
- This departs from the brief, which names Terraform. We keep the brief in [README.md](README.md) as written.
