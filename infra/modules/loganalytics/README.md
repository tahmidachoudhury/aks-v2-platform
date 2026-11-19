# loganalytics

**TL;DR:** One Log Analytics workspace, PerGB2018 with 30-day retention and a 0.5 GB daily cap, plus a diagnostic setting that sends two AKS control plane log categories to it: `kube-audit-admin` and `guard`. Container Insights stays off, because New Relic is the observability tool. Expected cost is £0 to £4 a month, and the cap holds the worst case to about £13.

The `platform` root calls this module, so the workspace and the diagnostic setting are destroyed every evening and created every morning. Filled by P3-19 (#45).

## Workspace

| Setting | Value | Why |
| --- | --- | --- |
| Name | `log-aks-v2-platform` | Platform variable `log_analytics_workspace_name`. See [Lifecycle](#lifecycle) for why it must not change |
| SKU | `PerGB2018` | Pay per GB. The only SKU for new workspaces apart from commitment tiers, which start at 100 GB a day |
| `retention_in_days` | 30 | The shortest Analytics retention, and free. Long enough to look back over a month of audit events |
| `daily_quota_gb` | 0.5 | Ingestion stops for the rest of the day when 0.5 GB arrive. Platform variable `log_analytics_daily_quota_gb` |
| `local_authentication_enabled` | `false` | No shared workspace keys. Queries use Entra ID, and diagnostic settings don't need keys |

### The daily cap

The cap is a backstop, not a budget. Once it is hit, logs stop until the cap resets. It resets at a fixed hour UTC that Azure picks per workspace, not at 9am. So a busy morning can leave the afternoon with no audit logs. That is the trade: a gap in the logs rather than a surprise on the bill. If the cap is hit often, see [Cost](#cost) before raising it.

### Lifecycle

`tofu destroy` soft-deletes the workspace. The provider's `permanently_delete_on_destroy` defaults to `false`, and this project leaves it there. When the morning apply creates a workspace with the same name, in the same resource group and region, within 14 days, Azure recovers the soft-deleted one with its data. So the 30 days of retention span the nightly rebuilds. Keep the name the same every day. A new name each day would leave the logs behind in an unrecovered workspace.

This is the documented Azure behaviour. It has not been tested on this project yet. If the morning apply ever fails on the workspace, check for a soft-deleted workspace with `az monitor log-analytics workspace list-deleted-workspaces`.

## Diagnostic setting

`aks-control-plane` sends three log categories to the workspace, in resource-specific mode (`log_analytics_destination_type = "Dedicated"`). The audit category lands in `AKSAuditAdmin`, and the others in `AKSControlPlane`. Those tables have a fixed schema, so rows are smaller and queries simpler than in `AzureDiagnostics`.

| Category | On | Why |
| --- | --- | --- |
| `kube-audit-admin` | Yes | API server audit events for every request except `get` and `list`. It shows who changed what in the cluster, which is what an audit log is for. `kube-audit` logs the reads too, which are most of the volume |
| `guard` | Yes | Entra ID sign-in and Azure RBAC decisions. Cluster access goes through Entra ID with `azure_rbac_enabled`, so this is where a denied `kubectl` call shows up |
| `karpenter-events` | No | Not in the documented AKS category list. An unknown category fails the whole diagnostic setting. Check `az monitor diagnostic-settings categories list --resource <cluster id>` on a live cluster, then add it if it is there |
| `kube-audit` | No | Every read as well as every write. Many times the volume of `kube-audit-admin` for little extra value |
| `kube-apiserver` | No | API server process logs. Noisy, and the control plane is Microsoft's to run |
| `kube-controller-manager`, `kube-scheduler`, `cloud-controller-manager` | No | Useful when debugging the control plane itself. Turn one on for a day if a problem points there |
| `cluster-autoscaler` | No | The cluster autoscaler is off. Node Auto Provisioning replaces it |
| `csi-azuredisk-controller`, `csi-snapshot-controller`, `csi-azurefile-controller` | No | Turn the disk ones on if a Postgres or Redis volume fails to attach. Azure Files is off |
| `fleet-*` | No | No Azure Kubernetes Fleet Manager here |
| Metrics (`AllMetrics`) | No | New Relic collects cluster metrics. Platform metrics are in Azure Monitor anyway, free, for 93 days |

To check the logs on a live cluster:

```bash
az monitor log-analytics query \
  --workspace "$(tofu -chdir=infra/platform output -raw log_analytics_workspace_guid)" \
  --analytics-query "AKSAuditAdmin | summarize count() by Verb | order by count_ desc"
```

The acceptance test is rows in `AKSAuditAdmin` and `AKSControlPlane` a few minutes after a `kubectl apply`. Run `Usage | where TimeGenerated > ago(1d) | summarize sum(Quantity) by DataType` to see the day's ingest by table, in MB.

### Key Vault diagnostics

Not added. The vault lives in `persistent`, and platform doesn't look it up yet. Once P3-13 is applied and platform reads the vault by name for P3-14, add a second diagnostic setting here for its `AuditEvent` category. It would be destroyed every night with the workspace, which is fine.

## No Container Insights

The cluster has no `oms_agent` block and no monitoring add-on, and this module creates no data collection rule. Container Insights would ship container logs, inventory and performance data to this workspace. That is the job [Observability](../../../docs/architecture-decisions.md#observability) gave to New Relic, whose free 100 GB covers it. Container logs in Log Analytics would cost £2.17 per GB after the first 5 GB, and the 0.5 GB cap would cut them off within hours. The workspace holds control plane logs only, which New Relic can't see.

## Cost

At UK South retail prices checked on 19 November 2026:

| Item | Rate |
| --- | --- |
| Analytics ingestion | First 5 GB a month free per billing account, then £2.17 per GB |
| Basic logs ingestion | £0.47 per GB, plus a charge per GB scanned by queries |
| Retention | Free for the first 30 days |

### Expected ingest

No measurement yet. This is an estimate to check against the `Usage` table after the first day.

- `kube-audit-admin` is the big one. Most of it is lease renewals: each node's kubelet updates its lease every 10 seconds, and each controller with leader election renews every 2 seconds or so. With about 7 nodes and 10 such controllers, at about 2.5 KB an event, that is roughly 0.2 to 0.4 GB in an 8-hour day.
- `guard` adds a few MB a day.

That is roughly 4 to 9 GB over a 22-day month.

| Case | Ingest a month | Cost a month |
| --- | --- | --- |
| Low | 4 GB | £0, inside the free 5 GB |
| Middle | 7 GB | About £4 |
| At the cap every day | 11 GB | About £13 |

The free 5 GB is per billing account. If another workspace on the same billing account already uses it, add about £11 to each row.

### Why Analytics and not Basic logs

The card suggests the Basic table plan. At the expected volume the saving is at most a few pounds a month: 7 GB is about £4 on Analytics after the free 5 GB, and about £3 on Basic if the free allowance doesn't apply to it. Basic saves more only near the cap, about £8. In return it limits queries and charges per GB scanned. It would also need the plan set on each table every morning, since the workspace is recreated. Analytics is simpler for little difference in cost.

### If ingest runs high

In order:

1. Check `Usage` and the top `Verb` and `ObjectRef` values in `AKSAuditAdmin`.
2. Drop lease updates with a workspace transformation rule on `AKSAuditAdmin`, which filters rows before they are billed.
3. Move `AKSAuditAdmin` to the Basic plan.
4. Only then raise the daily cap.
