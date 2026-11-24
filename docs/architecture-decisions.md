# Architecture Decisions

**TL;DR:** All six P0 decisions are settled. We use Service Bus Standard, Kustomize plus Helm, in-cluster Postgres, HPA with KEDA, New Relic and External Secrets Operator. Infrastructure is written in OpenTofu, not Terraform. Cluster egress uses the load balancer's outbound rules, not a NAT Gateway. The container registry is ACR Basic, and disks use platform-managed keys. Key Vault and state storage get private endpoints. The vault is firewalled to the operators' IPs, and state stays public behind Entra ID. TLS certificates come from Let's Encrypt through cert-manager's HTTP-01 solver, so cert-manager needs no Azure identity. An hourly CronJob snapshots the Postgres disk, and the snapshots last only until the evening destroy.

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
| [Egress](#egress)                  | Load balancer outbound rules, no NAT Gateway                    | Accepted | [#31](https://github.com/tahmidachoudhury/aks-v2-platform/issues/31) |
| [Container registry](#container-registry) | ACR Basic, Entra ID access only, no private endpoint | Accepted | [#36](https://github.com/tahmidachoudhury/aks-v2-platform/issues/36) |
| [Disk encryption](#disk-encryption) | Platform-managed keys, no Disk Encryption Set, no encryption at host | Accepted | [#46](https://github.com/tahmidachoudhury/aks-v2-platform/issues/46) |
| [Private endpoints](#private-endpoints) | Endpoints for Key Vault and state blob in platform. Vault firewalled to operator IPs, state public with Entra ID only | Accepted | [#40](https://github.com/tahmidachoudhury/aks-v2-platform/issues/40) |
| [TLS certificates](#tls-certificates) | cert-manager with Let's Encrypt, HTTP-01 through Traefik, no Azure identity | Accepted | [#51](https://github.com/tahmidachoudhury/aks-v2-platform/issues/51) |
| [Volume snapshots](#volume-snapshots) | Hourly CronJob, newest six kept, snapshots last the day | Accepted | [#61](https://github.com/tahmidachoudhury/aks-v2-platform/issues/61) |
| [Database migrations](#database-migrations) | golang-migrate in a Job, an Argo CD hook in its own Application from P7, expand/contract, forward only | Accepted | [#58](https://github.com/tahmidachoudhury/aks-v2-platform/issues/58) |

## Event bus

**Status:** Accepted

**Context:** The original authors wrote the services for Amazon SQS. order-service, payment-service and shipping-service publish events and the worker consumes them (see the [service catalogue event flow](service-catalogue.md#event-flow)). When this was decided, the publishers and the worker's receive loop were stubs, so the messaging code was written fresh for the bus we picked. Both are real now: the three publishers send with `azservicebus`, and the worker receives in PeekLock mode. The worker's event handlers still only log.

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
- Known gap: Service Bus has no private endpoint in this build. P3-14 drops Service Bus and keeps private endpoints for Key Vault and state storage (see [Private endpoints](#private-endpoints)). A production team should pay for Premium and add the private endpoint.
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
| api-gateway          | HPA    | CPU, 70% of request                                 | 2   | 4   | Stateless. Rate-limit counters live in Redis, so replicas share them. Public entry point, so two, one per node, in two zones where possible |
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
- scheduler runs as a one-replica StatefulSet, not a Deployment. A Deployment's rolling update runs the old and new pod together, and even `strategy: Recreate` only covers rollouts: a drain, an eviction or a delete starts the new pod while the old one is still stopping. Each job runs once at start-up (`runEvery` in `app/services/scheduler/main.go`), and `expire_reservations` lowers `products.reserved` without checking the reservation is still active, so two copies can release the same stock twice. A StatefulSet creates `scheduler-0` again only once the old pod has gone, whatever removed it ([apps](../k8s/apps/README.md#scheduler-one-statefulset-not-cronjobs)).
- Postgres connections set the low max replica counts. CPU does not.

**Consequences / trade-offs**

- Postgres connections break first under load. `SetMaxOpenConns` is hard-coded per service: 25 for order, inventory and payment, 15 for shipping, 10 for notification and dashboard, 5 for scheduler. Go opens connections only when needed, but at full load the minimums allow 205 and the maximums 315. Postgres allows 100 by default, so the StatefulSet sets `max_connections=350`: the maximums, a rollout's surge pod and the migration Job (see [data](../k8s/data/README.md#postgres-connections)).
- Options for the limit: raise `max_connections` in the StatefulSet and size its memory to match, add PgBouncer, or make the pool size configurable in code.
- HPA needs CPU requests on every Deployment.
- KEDA scales the worker on the queue's active message count. The receive loop is real (PeekLock, in `app/services/worker/main.go`), but the handlers only log, so for now more replicas add receive throughput and no downstream work.
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

## Egress

**Status:** Accepted

**Context:** The cluster's pods and nodes need outbound internet access for images, Service Bus and Let's Encrypt. The brief says to avoid a NAT Gateway if we can. Both platform subnets set `default_outbound_access_enabled = false`, so without an explicit outbound method nothing gets out.

**Decision:** `outbound_type = "loadBalancer"` with one managed outbound public IP. AKS adds an outbound rule to the cluster's Standard Load Balancer, and that is the only egress path. Traefik needs the same load balancer for ingress, so egress adds one public IP and nothing else. The SNAT detail is in the [aks module README](../infra/modules/aks/README.md#egress).

**Options compared**

| Option | Monthly cost | SNAT ports | For | Against |
| --- | --- | --- | --- | --- |
| Load balancer outbound rules (chosen) | About $4 extra for one public IP. The rule shares the load balancer Traefik needs ($18.25 for the first five rules). $0.005 per GB processed | 64,000 per public IP, split up front. 1,024 per node by default up to 50 nodes | Cheapest. No extra resource to manage | Fixed ports per node. A busy node can run out while others sit idle |
| NAT Gateway | About $33 plus $3.65 for its public IP, plus $0.045 per GB | 64,512 per public IP, given out on demand across the subnet. Up to 16 IPs | Best SNAT behaviour. No per-node split | About eight times the cost of the extra IP. The brief asks us to avoid it |
| User-defined routing through Azure Firewall | Basic about $288, Standard about $913, plus $0.065 or $0.016 per GB | Done by the firewall's public IPs | Filters egress by FQDN and logs every flow | Far over budget. Needs a firewall subnet, a route table and more public IPs |

Prices checked on 19 November 2026, UK South. Sources: the [Azure Retail Prices API](https://prices.azure.com/api/retail/prices), [AKS outbound types](https://learn.microsoft.com/en-us/azure/aks/egress-outboundtype), [Standard Load Balancer on AKS](https://learn.microsoft.com/en-us/azure/aks/load-balancer-standard), [load balancer SNAT](https://learn.microsoft.com/en-us/azure/load-balancer/load-balancer-outbound-connections), [NAT Gateway pricing](https://azure.microsoft.com/en-gb/pricing/details/azure-nat-gateway/).

**Consequences / trade-offs**

- No FQDN filtering on egress. A compromised pod can reach any internet address. A production team should route egress through a firewall.
- The outbound IP is stable for the life of the cluster but changes every morning, since the cluster is rebuilt. Anything that allow-lists our egress IP, such as the Service Bus IP firewall, must read it from the cluster each day.
- If SNAT ports run out, set `outbound_ports_allocated` or add a second outbound IP before reaching for a NAT Gateway.
- Node Auto Provisioning doesn't allow the outbound type to change after create. Changing it means a new cluster, which happens every morning anyway.

Ticket: [#31](https://github.com/tahmidachoudhury/aks-v2-platform/issues/31)

## Container registry

**Status:** Accepted

**Context:** Nine services need images in a registry that survives the nightly destroy, so the registry lives in the `persistent` root. The card asked for Premium with a private endpoint. The brief asks for private endpoints for ACR, Key Vault, Service Bus and the state storage account. The budget is about £100 a month.

**Decision:** Azure Container Registry Basic, with no private endpoint. The admin user and anonymous pull are off, so every pull and push needs an Entra ID token. Premium is the only tier with private endpoints and costs about $50 a month whether used or not, half the budget for one network path.

**Options compared**

| Option | Monthly cost | Private endpoint | Network rules | Included storage |
| --- | --- | --- | --- | --- |
| Basic (chosen) | About $5 | No | No | 10 GiB |
| Standard | About $20 | No | No | 100 GiB |
| Premium | About $51 | Yes | IP rules and private endpoint | 500 GiB |

Prices checked on 19 November 2026, UK South. Storage beyond the included amount is $0.10 per GB a month on every tier. Sources: [ACR pricing](https://azure.microsoft.com/en-gb/pricing/details/container-registry/), [ACR service tiers](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-skus), and the [Azure Retail Prices API](https://prices.azure.com/api/retail/prices).

**Consequences / trade-offs**

- Known gap: ACR has no private endpoint in this build. Image pulls from AKS cross the public endpoint, authenticated with Entra ID: AcrPull for the kubelet identity (P3-11) and AcrPush for CI (P3-21). A production team should pay for Premium and add the private endpoint.
- P3-14 keeps private endpoints for Key Vault and state storage. ACR joins Service Bus as a service with no private endpoint.
- Basic has no IP firewall either, so Entra ID is the only control. No admin user means no shared password to leak.
- Standard costs four times Basic and adds storage and throughput we do not need. It still has no private endpoint.
- 10 GiB holds many tags of nine small Go images. Add a retention or purge task if storage grows.

Ticket: [#36](https://github.com/tahmidachoudhury/aks-v2-platform/issues/36)

## Disk encryption

**Status:** Accepted

**Context:** Every managed disk in Azure is encrypted at rest. By default the key is platform-managed: Azure creates, stores and rotates it. The card asks whether to add a Disk Encryption Set (DES) with a customer-managed key (CMK) in Key Vault instead. The disks here are the 64 GB OS disks on each node and the Premium SSD volumes for Postgres and Redis. The cluster and its disks are rebuilt every morning, and the budget is about £100 a month.

**Decision:** Keep platform-managed keys. No DES, no CMK, and no encryption at host. The data is already encrypted at rest with AES-256. A CMK adds a key, an identity, two role grants and wiring on the cluster and the StorageClass, all to hold a key for disks that live eight hours. It also adds a new way to lose data: if the key is disabled, deleted or its access is removed, the disks can't be read and the nodes can't start.

**Options compared**

| Option | Monthly cost | What it adds | Setup | Risk |
| --- | --- | --- | --- | --- |
| Platform-managed keys (chosen) | £0 | AES-256 at rest on every disk | None. The default | None beyond the default |
| Encryption at host, platform keys | £0. No separate charge | Also encrypts the temp disk and the OS and data disk caches on the VM host. Data reaches Storage already encrypted | `host_encryption_enabled` on the system pool and `security.encryptionAtHost` on the AKSNodeClass. Needs the `Microsoft.Compute/EncryptionAtHost` feature registered on the subscription | Low. Set at node pool create only, which happens every morning anyway |
| DES with a CMK | Pennies. The DES is free. Key operations cost $0.03 per 10,000 for RSA 2048, $0.15 for larger keys. Automatic key rotation is $1 per rotation | We choose and rotate the key, and can revoke it to make every disk unreadable | A Key Vault key, a DES with its own identity, "Key Vault Crypto Service Encryption User" on the key, "Reader" on the DES for the cluster identity, `disk_encryption_set_id` on the cluster, `diskEncryptionSetID` on the StorageClass | High. A disabled or deleted key, or a lost role grant, stops nodes and volumes |
| DES with double encryption | As the CMK row | Two layers at rest: platform key and CMK (`EncryptionAtRestWithPlatformAndCustomerKeys`) | As the CMK row | As the CMK row |

Prices checked on 19 November 2026, UK South, from the [Azure Retail Prices API](https://prices.azure.com/api/retail/prices). Sources: [server-side encryption of managed disks](https://learn.microsoft.com/en-us/azure/virtual-machines/disk-encryption), [host-based encryption on AKS](https://learn.microsoft.com/en-us/azure/aks/enable-host-encryption), [BYOK for Azure disks in AKS](https://learn.microsoft.com/en-us/azure/aks/azure-disk-customer-managed-keys), [AKSNodeClass security settings](https://learn.microsoft.com/en-us/azure/aks/node-auto-provisioning-aksnodeclass#security-settings).

**Encryption at host on our sizes**

`az vm list-skus` reports `EncryptionAtHostSupported = True` in UK South for every size the cluster uses: `Standard_D2nls_v6` on the system pool, and `Standard_D2s_v6` and `Standard_D2nls_v6` in the Node Auto Provisioning pools. `Standard_D4s_v6` and `Standard_D4nls_v6` reported the same, but `general` no longer offers them since P6-04. Node Auto Provisioning has supported both encryption at host and DES since February 2026. The `EncryptionAtHost` feature shows `NotRegistered` on this subscription. Registering it is a one-off, subscription-wide step, in the same spirit as `resource_provider_registrations = "none"`, so it stays manual.

**Consequences / trade-offs**

- Disk data is encrypted at rest with keys we never see. Anyone with Azure access to the disk, such as a snapshot, reads it through Azure RBAC, not a key we control.
- Server-side encryption doesn't cover the temp disk or the disk cache on the VM host. Encryption at host does. The v6 sizes here have no temp disk, so only the cache is left out. The AKS page reads as if caches are covered by default, and the managed disk page says they are not, so treat the cache as uncovered.
- Encryption at host is the cheap upgrade if a later card wants it: register the feature, then set one flag on the system pool and one on the AKSNodeClass. No key, no identity and no Key Vault dependency.
- A production team with a compliance need to control or revoke keys should use a DES with a CMK, kept in the `persistent` root next to the vault. The vault already has purge protection, which a CMK requires.
- A DES with a CMK has to be set when the cluster is created. Moving to it later means a new cluster, which happens every morning here.

Ticket: [#46](https://github.com/tahmidachoudhury/aks-v2-platform/issues/46)

## Private endpoints

**Status:** Accepted

**Context:** The brief asks for private endpoints for ACR, Key Vault, Service Bus and the state storage account, with public access disabled or restricted on each. ACR Basic and Service Bus Standard have no private endpoints (see [Container registry](#container-registry) and [Event bus](#event-bus)), which leaves Key Vault and state. A private endpoint lives in a VNet, and the only VNet is the platform one, destroyed every evening. Three clients reach these two resources from outside it:

- The operators apply from a laptop. Every root reads and writes state, and a persistent apply writes Key Vault secrets through the data plane.
- CI runs on GitHub-hosted runners, as `id-gh-infra` and `id-gh-infra-plan`. Both read state, and the apply identity writes it. GitHub publishes about 5,600 IPv4 ranges for Actions runners (checked on 19 November 2026 from `api.github.com/meta`), and a job can come from any of them.
- The cluster, through External Secrets Operator, reads Key Vault. Nothing in the cluster uses state.

**Decision:** Both endpoints go in the platform root, in `snet-private-endpoints`, keyed like the zones, with their A records in the private DNS zones from P3-06. They are created each morning and destroyed each evening with the VNet. Each resource is then restricted as far as its outside clients allow:

- Key Vault: public network access stays on, the firewall denies by default, and only the operators' public IPs get through. They are set in the persistent root's gitignored `terraform.tfvars`, never in Git. No trusted-service bypass. The cluster uses the endpoint, which the firewall doesn't apply to.
- State account: public network access stays on with no IP firewall. Shared keys have been off since bootstrap, so every request needs an Entra ID token and a Storage Blob Data role. The endpoint gives in-VNet clients a private path, and nothing more.

**Options compared**

| Option | Key Vault | State | Why not |
| --- | --- | --- | --- |
| Disable public access on both | Endpoint only | Endpoint only | The laptop can no longer write secrets. Every root loses its state, including the evening destroy and the morning apply that recreate the endpoint. Nothing could ever rebuild it |
| Firewall both to the operators' IPs | Operators and endpoint | Operators and endpoint | CI can no longer read state. 5,600 runner ranges won't fit: a storage account takes 400 IP rules |
| Vault firewalled, state open with Entra ID only (chosen) | Operators and endpoint | Any address, with a token and a role | |
| Endpoints only, both left open | Any address, with a token and a role | As chosen | Restricting the vault costs nothing and only CI's future apply needs a way in |
| A persistent VNet with self-hosted runners | Endpoint only | Endpoint only | Needs a VNet that survives the night, a runner VM or a paid GitHub plan for Azure private networking, and a jumpbox or VPN for the laptop. This is the production answer, but too much for this build |

**CI access path**

- Pull request plans run as `id-gh-infra-plan` with `-refresh=false`, so they never touch the vault's data plane. They read state over the public endpoint with Storage Blob Data Reader. Nothing changes for them.
- Apply jobs run as `id-gh-infra` and read and write state the same way, with Storage Blob Data Contributor.
- A persistent apply in CI (P8-01) refreshes and writes vault secrets, so the vault firewall blocks it. That job must let its own runner IP in for the run. The operators' ranges are only in the laptop's gitignored tfvars, so CI reads them from a repository secret, `KEY_VAULT_ALLOWED_IP_RANGES`, kept as a JSON list. The job adds the runner IP with `az keyvault network-rule add`, sets `TF_VAR_key_vault_allowed_ip_ranges` to the secret's list plus the runner IP so the apply keeps both, and removes the runner rule in an `always()` step at the end. Without the secret, the apply would replace the operators' rules with the runner IP alone, and the cleanup step would then leave the vault with no rules at all. The removal happens outside OpenTofu, but the next refresh reads the vault without that rule, which matches the config, so no drift shows. The secret has to change whenever an operator's IP does, like the tfvars. `id-gh-infra` holds Contributor on `rg-aks-v2-persistent`, which covers the firewall. Until P8-01, apply persistent by hand.
- CI can't apply platform yet (see the [github-oidc README](../infra/modules/github-oidc/README.md#known-gap-the-platform-resource-group)). When it can, it also needs approval rights for private endpoint connections on the state account, which sits in `rg-aks-v2-tfstate`, outside its Contributor scope.

**Consequences / trade-offs**

- Still public: the state account to anyone with an Entra ID token and a blob data role, the vault to the operators' IPs, ACR to anyone with a token and AcrPull or AcrPush, and Service Bus to the cluster's egress IP.
- Overnight, with no endpoints, the vault is reachable only from the operators' IPs. Nothing in Azure reads it overnight, since the cluster is gone.
- An operator whose home IP changes gets a 403 from the vault. `tofu plan` in persistent fails on the secrets until the new IP is added, by hand first with `az keyvault network-rule add`, then in `terraform.tfvars`.
- Apply platform before persistent when introducing this. The endpoint must exist before the firewall closes, or External Secrets Operator loses the vault until the next platform apply.
- While the state endpoint exists, a laptop whose DNS server hosts its own `privatelink.blob.core.windows.net` zone can't resolve the state account, and no root can load state. The fix is a per-host resolver override on that machine. See [State account DNS](../infra/platform/README.md#state-account-dns). Dropping the state endpoint would also fix it, but the brief asks for one.
- Two endpoints at $0.01 an hour each cost about $3.50 a month on the 176-hour schedule, plus $0.01 per GB in each direction.

Prices checked on 19 November 2026 from the [Azure Retail Prices API](https://prices.azure.com/api/retail/prices). Sources: [Key Vault network security](https://learn.microsoft.com/en-us/azure/key-vault/general/network-security), [storage account network rules](https://learn.microsoft.com/en-us/azure/storage/common/storage-network-security), [private endpoint DNS](https://learn.microsoft.com/en-us/azure/private-link/private-endpoint-dns).

Ticket: [#40](https://github.com/tahmidachoudhury/aks-v2-platform/issues/40)

## TLS certificates

**Status:** Accepted

**Context:** The brief asks for cert-manager with Let's Encrypt, and for the dashboard to be reachable over HTTPS at a real DNS name. Let's Encrypt proves control of a name with one of two challenges. HTTP-01 fetches a token from the name over port 80. DNS-01 reads a TXT record that cert-manager writes into the zone. Traefik is the ingress controller, ExternalDNS writes A records into the Azure DNS zone, and the cluster is rebuilt every weekday, so every certificate is issued fresh each morning.

**Decision:** HTTP-01 through Traefik, with a staging and a production ClusterIssuer. cert-manager starts a solver pod and a temporary Ingress for each challenge, and Let's Encrypt reaches it through Traefik's public IP, which ExternalDNS has already put in DNS. cert-manager needs no Azure access, so it gets no managed identity, no federated credential and no role on the zone.

**Options compared**

| Option | Azure setup | Covers | Depends on | Against |
| --- | --- | --- | --- | --- |
| HTTP-01 through Traefik (chosen) | None | Named hosts on the public load balancer | Port 80 open, the A record resolving, Traefik routing the challenge | No wildcards. No entrypoint-level HTTPS redirect, since that would catch the challenge |
| DNS-01 through Azure DNS | An identity in persistent, a federated credential in platform, DNS Zone Contributor on the zone | Any name in the zone, wildcards included, reachable or not | Workload identity and the Azure DNS API | A second component with write access to the zone. More to build and nothing the brief needs |

**Consequences / trade-offs**

- One less identity with write access to the public zone. ExternalDNS stays the only workload that can change it.
- A certificate can't be issued until ExternalDNS has written the A record and it resolves publicly. cert-manager's own self-check waits for that, so the first certificate each morning takes a minute or two longer.
- No wildcard certificates. Each host on an Ingress gets its own order. The brief needs one host behind one Ingress.
- Let's Encrypt production allows 5 new certificates for the same set of names every 7 days. The certificate Secret goes with the cluster every evening, so one host uses all five on the weekday schedule. DNS-01 doesn't change this: the limit counts certificates, not challenges. Day to day, Ingresses use `letsencrypt-staging`, and production is for demos and grading. Keeping the Secret overnight, for example in Key Vault through External Secrets, would allow production every day. The [cert-manager README](../k8s/cert-manager/README.md#rate-limits) has the detail.
- HTTP-01 proves control of a name by answering on the IP its A record points at. The records stay in the zone overnight, so Traefik's IP lives in the persistent root and is never released. A released IP could be given to another Azure customer, who could then pass an HTTP-01 challenge and get a certificate for our names. Holding the IP costs about $3.65 a month instead of $0.88 on the schedule. See [Ingress public IP](../infra/platform/README.md#ingress-public-ip).
- Moving to DNS-01 later is a small change: add `cert-manager` to the workload identities in persistent with DNS Zone Contributor on the zone, a federated credential in platform, and an `azureDNS` solver on the issuers. See the [workload-identity README](../infra/modules/workload-identity/README.md#cert-manager).

Checked on 20 November 2026. Sources: [Let's Encrypt challenge types](https://letsencrypt.org/docs/challenge-types/), [Let's Encrypt rate limits](https://letsencrypt.org/docs/rate-limits/), [cert-manager HTTP-01](https://cert-manager.io/docs/configuration/acme/http01/), [cert-manager Azure DNS](https://cert-manager.io/docs/configuration/acme/dns01/azuredns/).

Ticket: [#51](https://github.com/tahmidachoudhury/aks-v2-platform/issues/51)

## Volume snapshots

**Status:** Accepted

**Context:** P5-07 asks for scheduled disk snapshots with retention, and the brief grades a snapshot taken and restored. Postgres holds the only durable state. Every evening `make down` destroys the platform root: the cluster, the node resource group and every disk and snapshot in it. The Azure Disk CSI driver stores a snapshot in its source disk's resource group unless the VolumeSnapshotClass sets `resourceGroup`. For a disk the cluster provisioned, that is the node resource group. The budget is £100 a month, and the nodes take about £60 of it.

**Decision:** An in-cluster CronJob snapshots the Postgres PVC every hour and keeps the newest six. The snapshots stay in the node resource group and go with the cluster each evening. They are same-day recovery points against a bad migration or a mistake during the day, and `make restore-drill` proves the restore. They are not a backup that outlives the cluster. The detail is in [k8s/backup](../k8s/backup/README.md).

**Options compared**

| Option | Monthly cost | Survives the destroy | Setup | Pruning |
| --- | --- | --- | --- | --- |
| CronJob, snapshots in the node resource group (chosen) | Pennies. Incremental snapshots at $0.053 per GB a month on changed blocks, deleted each evening | No | A ServiceAccount, a Role and a CronJob in `data` | The CronJob, by count |
| CronJob, snapshots in a persistent resource group | Pennies, but they build up. Each morning's disk is new, so its first snapshot holds every used block | Yes | A second VolumeSnapshotClass with `resourceGroup`, and a role for the cluster identity on that group in OpenTofu. A restore on a new cluster needs a hand-made VolumeSnapshotContent pointing at the Azure snapshot | The CronJob sees only the current cluster's snapshots. Older days need pruning outside the cluster, with `az` or a script, or they pile up |
| Azure Backup for AKS, vault in persistent | $12 (about £9) per protected namespace a month, plus snapshot and vault storage | Yes | A Backup vault and policy in persistent, a storage account for the extension, and the Backup extension and trusted access on every morning's cluster | The backup policy |
| Velero with the Azure plugin | Blob storage, pennies at this size | Yes for manifests. Volume data needs its data mover, which copies it to blob storage | A Helm install, a storage account and a workload identity, all applied each morning | The schedule's TTL |

Prices checked on 20 November 2026, UK South, from the [Azure Retail Prices API](https://prices.azure.com/api/retail/prices). Sources: [Azure Disk CSI VolumeSnapshotClass parameters](https://github.com/kubernetes-sigs/azuredisk-csi-driver/blob/master/docs/driver-parameters.md), [incremental snapshots](https://learn.microsoft.com/en-us/azure/virtual-machines/disks-incremental-snapshots), [Azure Backup for AKS](https://learn.microsoft.com/en-us/azure/backup/azure-kubernetes-service-backup-overview), [Velero Azure plugin](https://github.com/vmware-tanzu/velero-plugin-for-microsoft-azure).

**Why same-day is enough here**

- The brief grades a snapshot taken and restored, and the card asks for snapshots on a schedule with old ones pruned. A same-day schedule meets both, and shows both working within a day.
- Snapshots that outlive the cluster are only worth their setup if the data is meant to outlive the cluster. That is a separate decision, below.
- Azure Backup for AKS would take about 9% of the budget, plus an extension to install and trust on every morning's cluster.

**Consequences / trade-offs**

- A restore can go back at most to that morning. Nothing survives the evening destroy.
- P5-01 ([#55](https://github.com/tahmidachoudhury/aks-v2-platform/issues/55)) decided that the Postgres data does not survive the destroy: Postgres starts empty each morning, so there is nothing to carry across days, and same-day snapshots match it. If that changes and its disk moves outside the node resource group, the driver puts the snapshots in that disk's group, so they survive the destroy too, but as orphans. The VolumeSnapshots go with the cluster, so `deletionPolicy: Delete` never fires and the CronJob never sees them to prune. In that case, set `resourceGroup` on the VolumeSnapshotClass and prune old days outside the cluster, as in the second option, or move to Azure Backup for AKS.
- Snapshots are crash-consistent. Postgres keeps its WAL on the same disk, so a restore is a crash recovery. The CronJob has no `pods/exec` rights, so it can't run `CHECKPOINT` first, and doesn't need to.
- Redis is not snapshotted. It holds only api-gateway's rate-limit counters.
- A production team would use Azure Backup for AKS with a vault-tier copy, or Flexible Server's point-in-time restore (see [Postgres](#postgres)).

Ticket: [#61](https://github.com/tahmidachoudhury/aks-v2-platform/issues/61)

## Database migrations

**Status:** Accepted

**Context:** Seven services share the `orders` database. Five of them (order, inventory, payment, notification and shipping) create their own tables at startup in `migrate()`, with `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS`, and notification-service seeds its templates. scheduler and dashboard-api read tables the others create. Nothing records a schema version, nothing orders changes across services, and an `IF NOT EXISTS` statement can't change a table that already exists. The brief asks how schema changes get applied and rolled back, and how to add a column to `orders`, which dashboard-api reads, without downtime.

**Decision:** A Kubernetes Job in `apps` runs [golang-migrate](https://github.com/golang-migrate/migrate) over numbered SQL files in [`k8s/migrations/sql/`](../k8s/migrations/README.md), shipped in a ConfigMap. It connects as `app`, the role that owns the database and that the services use. From P7 Argo CD runs it as a Sync hook in its own `migrations` Application, in the wave before the services. Postgres starts empty every morning, and at bootstrap a failed migration holds the services back. Day to day, a migration merges before the release that needs it. Every change is expand/contract and rolls forward. There are no down migrations.

The first migration is a baseline: the five `migrate()` functions copied verbatim. The services keep their `migrate()` for now, unchanged. On any database the two give the same schema, so neither breaks the other.

**Where it runs**

| Option | Runs | Failure stops the rollout? | Against |
| --- | --- | --- | --- |
| Each service at startup (today) | Every pod start, in five services | No. A failing pod crashes while the old ReplicaSet keeps serving | No version or order. Each pod start takes a brief `SHARE` lock on every table it owns. The services' role must own the tables |
| Init container per Deployment | Every pod start | Partly. New pods sit in `Init:CrashLoopBackOff` mid-rollout | Seven services and their replicas race for one shared schema, and an HPA scale-out reruns it. No obvious image to carry the SQL |
| Job, Argo CD hook in its own Application (chosen) | Once per change to the SQL, in the wave before the services | At bootstrap, yes: the root holds the services back. Day to day, by merging the migration first | After bootstrap the order is a rule, not a gate |

**Which tool**

| Option | Migrations | Locking | Packaging | Against |
| --- | --- | --- | --- | --- |
| golang-migrate (chosen) | Plain SQL files, numbered | Postgres advisory lock on every run | Official image, about 25 MB | A failed file leaves the version marked dirty, cleared by hand. No checksum of applied files |
| goose | SQL or Go files | An advisory lock is a Go library option, not the CLI default | Official image | No lock out of the box |
| Flyway | SQL files, checksummed | Locks while it migrates | A JVM image of several hundred MB | Heavy for a 64Mi pod. Some features need a paid edition |
| A `psql` script and our own version table | SQL files | Whatever we write | The `postgres` image | We would build and test what the tools above already do |

golang-migrate locks by default, ships a small image, and reads the same SQL a developer would type into `psql`. Its Postgres driver is `lib/pq`, which the services already use.

**Packaging:** a ConfigMap generated by Kustomize from `sql/`. A migration then needs no CI build and no registry push, and the SQL sits next to the manifest that runs it. A ConfigMap holds 1 MiB, ample for SQL. An image of our own would version the SQL with a tag, but needs a pipeline for no gain at this size.

**Locking and retries**

- golang-migrate holds a Postgres advisory lock for the whole run, so two runs never apply at once. A second run waits up to 60 seconds, then finds nothing to do.
- `BeforeHookCreation` deletes any earlier `db-migrate` Job before each sync creates a new one, so every sync runs it. A finished Job left from before a database reset would otherwise make the run a no-op. golang-migrate is idempotent, so rerunning costs nothing.
- Each file runs in one transaction. A failure leaves no partial change.
- Each file starts with `SET lock_timeout = '10s';`. A DDL statement that can't get its table lock gives up rather than queue behind a long transaction, which would block every query queued behind it. The setting is in the files, not the connection URL, because in the URL it also caps golang-migrate's wait for the advisory lock at 10 seconds.
- `backoffLimit: 2` retries only failures before any file runs: a Postgres restart, or the advisory lock not freed in time. It can't recover a failure inside a file. golang-migrate marks the version dirty before running the file, so every retry stops at `Dirty database version N`, and that needs the manual recovery below. `activeDeadlineSeconds: 300` fails a Job that hangs.
- The pod meets Pod Security `restricted`: user 65532, read-only root, no capabilities, no service account token. It requests 10m CPU and 32Mi, inside the `apps` headroom of about 210m left after the services' 40m budget.

**Expand and contract: add a column to `orders`**

The services name their columns in every query and insert, with no `SELECT *`, so a new column breaks no running code. The migration runs before any pod changes, so the column always exists before code that uses it. Adding `gift_message`, which order-service writes and dashboard-api reads, takes these steps:

1. **Expand, release N.** `000002_orders_add_gift_message.up.sql`: `ALTER TABLE orders ADD COLUMN gift_message TEXT;`. Nullable with no default, it changes only the catalog and holds its lock for milliseconds. The release also ships order-service writing the column and dashboard-api reading it with `COALESCE`, since rows from before, and from old pods mid-rollout, hold `NULL`. The migration merges and syncs first, then the code, which rolls the two Deployments one pod at a time. Old and new pods both work against the new schema.
2. **Backfill, if needed, release N+1.** A separate migration, or a scheduler job for a large table, fills old rows in batches. A single `UPDATE` of every row would lock them all.
3. **Contract, release N+2 or later.** Tighten only once no running release, and no release you might roll back to, writes `NULL`. Add `CHECK (gift_message IS NOT NULL) NOT VALID`, then `VALIDATE CONSTRAINT` in its own file, so the check scans the table without blocking writes. Dropping or renaming a column follows the same pattern: stop using the old column in release N, drop it in N+1.

Every migration must work with the release that is running and with the one before it, the rollback target. A change that breaks this goes in two releases.

**Rollback**

- **The application.** Roll back the image: an Argo CD rollback or a revert of the tag. The schema stays expanded and the old code ignores the new column. Rolling back the code never needs a schema rollback.
- **The schema.** Forward only. Undoing a change is a new migration that contracts it, once no release uses it. A down migration would run in the same window as the code rollback and could drop data that new code had already written.
- **A failed migration.** Its transaction rolls back, so the schema is unchanged and the services were never rolled out. golang-migrate marks the version dirty. Fix the file, clear the dirty version and run again. For a later file that is one `UPDATE` back to the previous version. For a failed baseline it is a `DELETE` of the row, as version 0 isn't a migration and golang-migrate refuses it. The steps are in [When a migration fails](../k8s/migrations/README.md#when-a-migration-fails).
- **Bad data from a migration that worked.** Restore the Postgres volume from a snapshot. That loses writes since the snapshot, so it is the last resort.

**Consequences / trade-offs**

- The services' `migrate()` stays until a follow-up removes it. Until then the Job must connect as the role the services use, `app`. A service connecting as a role that doesn't own the tables exits at startup with `must be owner of table`, since `CREATE INDEX IF NOT EXISTS` needs ownership even when the index exists. Running the Job as the superuser would make `postgres` the owner and break the services the same way.
- The password reaches the Job as `PGPASSWORD`, not inside a URL, so it needs no escaping.
- The Job sits in its own Application in the wave before the services, not in theirs: a hook only gates its own Application, and each service is its own Application. At bootstrap the root's health check holds the services until it succeeds. Day to day the rule is to merge a migration before the release that needs it, which expand/contract makes safe.
- Only the Job is a hook. An Application made only of hooks never syncs, since Argo CD leaves hooks out of the sync status and auto-syncs only when it is OutOfSync. The ConfigMap, ExternalSecret and NetworkPolicy are ordinary resources a wave earlier, and the ConfigMap's hashed name makes a new migration sync. See the [migrations README](../k8s/migrations/README.md#under-argo-cd).
- Each migration takes two edits: the SQL file, starting with the `SET lock_timeout` line, and its line in `kustomization.yaml`. A file left out of the kustomization never runs and nothing warns.
- golang-migrate keeps no checksum, so editing an applied file is never detected. The README forbids it.
- A long `CREATE INDEX` must be `CONCURRENTLY`, which can't run in a transaction, so it goes in a file of its own.

Checked on 20 November 2026. Sources: [golang-migrate Postgres driver](https://github.com/golang-migrate/migrate/tree/master/database/postgres), [golang-migrate FAQ](https://github.com/golang-migrate/migrate/blob/master/FAQ.md), [ArgoCD resource hooks](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_hooks/), [Postgres `ALTER TABLE`](https://www.postgresql.org/docs/16/sql-altertable.html), [Postgres `lock_timeout`](https://www.postgresql.org/docs/16/runtime-config-client.html). The lock, the lock timeout, dirty versions, the baseline and the `app` ownership were tested against `postgres:16`. See the [migrations README](../k8s/migrations/README.md#validation).

Tickets: [#58](https://github.com/tahmidachoudhury/aks-v2-platform/issues/58), [#59](https://github.com/tahmidachoudhury/aks-v2-platform/issues/59)
