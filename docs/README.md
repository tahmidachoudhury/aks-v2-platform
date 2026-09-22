# AKS v2 - Order Fulfillment Platform

A multi-service order platform on Azure Kubernetes Service. Nine services, one cluster, stateful workloads running on Kubernetes. The application code is provided. You build everything else.

## Services

| Service | Description |
|---|---|
| api-gateway | Auth, rate limiting, routes requests to internal services |
| order-service | Order lifecycle and state machine |
| inventory-service | Stock management and reservations |
| payment-service | Payment processing, refunds, ledger |
| notification-service | Email and SMS dispatch |
| shipping-service | Shipments, tracking, carrier webhooks |
| worker | Service Bus consumer, orchestrates cross-service events |
| scheduler | Cron jobs (expired reservations, abandoned orders, retries) |
| dashboard-api | Admin dashboard UI, analytics and reporting |

Read the source code. Environment variables, endpoints and data models are in the code.

> Note: the services were originally written against AWS SQS. Moving to Azure means the four event-producing services and the worker need their messaging clients swapped for the Azure Service Bus SDK (or a Kafka client, if you take the Strimzi path). Budget for this.

## Your Job

Write the Dockerfiles. Write the Terraform. Write the Kubernetes manifests. Write the CI/CD pipeline. Deploy all nine services to AKS, with PostgreSQL and Redis running in-cluster on persistent volumes.

## Requirements

- AKS (1.33 or above, latest GA version AKS supports) with a system node pool and user node pools spread across 3 availability zones
- Node Auto Provisioning (NAP, the Karpenter-based provisioner on AKS) for node autoscaling. The Cluster Autoscaler is fine to compare against, but NAP is the default you should be reaching for.
- Nine Deployments behind a single Ingress with TLS, routed to the right backend
- PostgreSQL on a StatefulSet with a 20Gi PVC (Azure Disk Premium SSD, encrypted)
- Redis on a StatefulSet with AOF persistence on a 10Gi PVC (Azure Disk Premium SSD, encrypted)
- Azure Disk CSI Driver, a Premium SSD StorageClass set as default (consider zone-redundant `Premium_ZRS`), a VolumeSnapshotClass configured
- Azure Service Bus queue with dead-lettering (`max_delivery_count`) as the event bus. If you would rather run in-cluster Kafka via the Strimzi operator, that is also accepted. Event Hubs with its Kafka endpoint is a managed middle ground. Pick one path, defend it.
- Azure Container Registry with one repository per service, AKS granted `AcrPull` via the kubelet identity
- VNet with private subnets. Avoid a NAT Gateway if you can. Use private endpoints for ACR, Key Vault, Service Bus and the state storage account.
- Secrets sourced from Azure Key Vault via External Secrets Operator or the Secrets Store CSI Driver (AKS `azure-keyvault-secrets-provider` add-on). Not hardcoded, not in env files.
- Traefik as the Ingress controller, fronted by an Azure Standard Load Balancer. ingress-nginx is retired (no releases, no security fixes after March 2026) so do not pick it. cert-manager with Let's Encrypt for TLS. ExternalDNS managing Azure DNS records.
- GitHub Actions with OIDC federated to Microsoft Entra ID. No client secrets, no long-lived Azure credentials.
- ArgoCD in the cluster, App-of-Apps pattern, auto-sync on the dev overlay
- Zero-downtime rollouts with rollback on failure
- Least-privilege RBAC with Microsoft Entra Workload ID for every service that touches Azure (one user-assigned managed identity per service, federated to its Kubernetes ServiceAccount)
- Terraform with remote state in an Azure Storage Account (blob backend, lease-based locking)
- Multi-stage Docker builds
- Container image scanning before deploy

## Deliverables

- [ ] Dockerfiles, one per service
- [ ] Terraform for all infrastructure (resource groups, VNet, AKS, managed identities and role assignments, ACR, Service Bus, Key Vault, DNS, add-ons)
- [ ] Kubernetes manifests (Kustomize or Helm)
- [ ] ArgoCD Applications wiring the cluster to your manifests repo
- [ ] GitHub Actions pipelines for infra and app, separated
- [ ] Working deployment with all services healthy and the end-to-end flow functional
- [ ] Dashboard UI reachable over HTTPS at a real DNS name, connected to all services
- [ ] README covering the sections below

## What Your README Must Cover

This is not optional. Your README is part of the submission.

**Architecture decisions.** What you built, why you built it that way, what you traded off. Why StatefulSets for Postgres instead of Azure Database for PostgreSQL Flexible Server. Why Kustomize over Helm or the other way round.

**Deployment pipeline.** A developer pushes a change to the payment service. Walk through exactly what happens from commit to live traffic. How do app deploys and infra changes stay out of each other's way? What triggers what? Where does ArgoCD fit in that flow?

**Secrets management.** Nine services need database credentials, API keys, JWT secrets. How do they get from Key Vault into a pod? What happens when you rotate a secret?

**Storage.** Postgres holds the only durable state in the system. How is the PVC backed? Encrypted (platform-managed keys or customer-managed keys via a Disk Encryption Set)? Snapshotted? What is your restore procedure and have you actually tested it?

**Scaling strategy.** Which services scale, on what metric, with HPA or KEDA? What stays fixed? What breaks first under load?

**Database migrations.** Seven services share one database. How do schema changes get applied? Job, init container, manual? What about rollback?

**Observability stack choice.** New Relic versus self-hosted kube-prometheus-stack (Prometheus, Grafana, Alertmanager) versus Azure Managed Prometheus with Azure Managed Grafana. Compare cost at this scale, ingest pricing, retention, operational burden, vendor lock-in, and what you would pick for a real team.

## Things to Consider

These are not requirements. They are the kind of problems you will hit in production. How you handle them is up to you.

- Your Postgres pod gets rescheduled to a different availability zone. What happens to its PVC? Does an LRS disk follow it? Does ZRS change the answer?
- The worker processes events from Service Bus. What happens to messages that fail three times?
- The payment service goes down for two minutes. What happens to in-flight orders?
- You need to add a column to the orders table. The dashboard service reads from that table. How do you deploy both without downtime?
- A junior dev pushes a bad image for the notification service. How quickly can you roll back without affecting the other eight? Does ArgoCD help or hurt here?
- Spot node pools save money. Which workloads tolerate eviction? Which absolutely cannot?
- Your logging pipeline ingests from nine services plus the AKS control plane diagnostic logs. What does that cost per month in Log Analytics or New Relic? Is there a cheaper way (Basic logs tier, Loki, sampling)?
- You rotate the database password. Do all nine deployments restart? Is there a way to avoid that?
- A single-zone managed disk becomes a problem when the zone goes down. What is your answer?

## Local Development

```bash
docker compose up --build
```

## Advanced

Not required for submission. These will set your project apart.

**Observability.** It is 2am. Orders are failing. You are on call. You need to answer four questions fast: which service is the problem, when did it start, what changed, who is affected. If your setup cannot answer those in under 10 minutes without `kubectl exec`, it is not production-ready. kube-prometheus-stack or New Relic gives you the building blocks. RED metrics for the API layer. Saturation metrics for the data layer. Dashboards grouped by service, not by pod. Alerts that mean something, routed somewhere a human will see them. A way to follow one order across all nine services.

**Service mesh.** Istio (AKS Istio add-on) or Linkerd. mTLS between every service. Authorization policies that block lateral movement. Traffic shifting for canary releases.

**Distributed tracing with OpenTelemetry.** Run an OTel collector. Instrument the Go services (the SDK is small). Send spans to Tempo, Jaeger or New Relic via OTLP. Follow a single order across api-gateway, order-service, payment-service, shipping-service and worker. This is the bit that pays you back at 2am.

**Gateway API instead of Ingress.** Traefik supports Gateway API CRDs (GatewayClass, Gateway, HTTPRoute). The Kubernetes community is moving in this direction now that ingress-nginx is gone. Build the platform with Gateway resources instead of Ingress.

**Backup and disaster recovery.** Velero with the Azure plugin, or Azure Backup for AKS, for cluster-level backup. Managed disk snapshots on a schedule. Restore in a fresh cluster and prove the application comes back up with its data intact.

**Chaos drill.** Kill a Postgres pod live during your demo. Watch the StatefulSet bring it back. Watch the application recover. One concrete drill, not a chaos platform.

## Grading

- All nine services running and healthy on AKS
- End-to-end flow works through the dashboard UI (create order -> reserve inventory -> process payment -> ship -> deliver)
- Postgres and Redis on StatefulSets with persistent volumes that survive pod restarts
- Volume snapshot taken and restored successfully
- Application reachable over HTTPS at a real DNS name
- Pipeline deploys only what changed
- Secrets not hardcoded anywhere
- No long-lived Azure credentials
- README covers all required sections with real decisions, not filler
- You can explain every resource you created

Tear down when done. AKS node pools, managed disks, the Load Balancer, public IPs, Log Analytics ingestion and data transfer add up fast.

## Found a bug?

The services have rough edges (see the audit notes in the team review channel). If you spot a real bug, open a PR against this repo. Include screenshots of the bug reproducing, your fix, and the same scenario working after the fix.
