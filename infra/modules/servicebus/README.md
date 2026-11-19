# servicebus

**TL;DR:** One Service Bus Standard namespace with the `order-events` queue. A message that fails three deliveries, or sits unreceived for an hour, moves to the dead-letter queue. SAS keys are off, so every client signs in with Entra ID. An IP firewall lets in only the cluster's egress IP. It costs about £2 a month on the 9am to 5pm schedule.

The `platform` root calls this module, so the namespace is destroyed every evening and created every morning. Messages left in the queue do not survive the night. Filled by P3-12 (#38). The tier and the lack of a private endpoint were decided in [Event bus](../../../docs/architecture-decisions.md#event-bus).

## Namespace

| Setting | Value | Why |
| --- | --- | --- |
| SKU | Standard | Queues with dead-lettering at about $10 a month against $677 for Premium. Basic has no IP firewall |
| Name | `servicebus_namespace_name` in platform | Globally unique, so it is a required variable with no default. See below |
| `local_auth_enabled` | `false` | No SAS keys and no connection strings. Pods use Workload Identity with `DefaultAzureCredential` |
| `minimum_tls_version` | `1.2` | The provider default, the only value it allows |
| Public network access | On | Standard has no private endpoint, so the public endpoint is the only way in |
| Firewall default action | Deny | Only the addresses in `allowed_ip_addresses` get through |
| `ip_rules` | The cluster's egress IP | See [Firewall](#firewall) |
| Trusted services | Off | Standard doesn't offer them, and nothing here needs them |

### The name stays the same every day

The manifests set `SERVICEBUS_NAMESPACE` to the FQDN, `<name>.servicebus.windows.net`. The name is a fixed variable, so each morning's namespace has the same FQDN and the manifests never change. Set it in a gitignored `terraform.tfvars`, as in [`terraform.tfvars.example`](../../platform/terraform.tfvars.example). Check a name is free first:

```bash
az servicebus namespace exists --name <name>
```

The variable rejects names Azure would refuse: 6 to 50 letters, digits and hyphens, starting with a letter, ending with a letter or digit, and not ending in `-sb` or `-mgmt`. Azure releases the name when the namespace is deleted, so the morning apply takes it back. Someone else could claim it overnight. That is unlikely for a name like this, and the apply would fail loudly if they did.

## Queue

The settings match the local emulator in [`app/servicebus-emulator/Config.json`](../../../app/servicebus-emulator/Config.json), so a message behaves the same in compose and on AKS.

| Setting | Value | Why |
| --- | --- | --- |
| Name | `order-events` | What the app reads from `SERVICEBUS_QUEUE`. See the [service catalogue](../../../docs/service-catalogue.md). Platform variable `servicebus_queue_name` |
| `max_delivery_count` | 3 | The worker abandons a message when its handler fails. The third failed delivery moves it to the DLQ. The default of 10 would retry a broken message for longer than is useful |
| `default_message_ttl` | `PT1H` | A message nobody completes within an hour expires. The default is 14 days, longer than the namespace lives |
| `dead_lettering_on_message_expiration` | `true` | An expired message goes to the DLQ instead of being dropped, so a worker outage leaves a trace |
| `lock_duration` | `PT1M` | The Azure default. The handlers take milliseconds, and a minute gives a stuck one time before Service Bus redelivers |
| Duplicate detection, sessions, partitioning | Off | Nothing in the app uses them. Duplicate detection and sessions can only be set when the queue is created |

The dead-letter queue is `order-events/$deadletterqueue`. Nothing reads it yet.

## Firewall

[Egress](../../../docs/architecture-decisions.md#egress) leaves the cluster through one managed public IP on its load balancer. The IP is new every morning, because the cluster is. So the namespace can't hold a fixed address.

The [aks module](../aks/README.md) outputs `outbound_ip_addresses`. It reads the IP resource IDs from `effective_outbound_ips` on the cluster, then looks up each address with a `data "azurerm_public_ip"`. Platform passes the result to this module as `allowed_ip_addresses`. In the morning apply the order is:

1. The cluster is created, with its outbound IP.
2. The data source reads the address. Its inputs are unknown until the cluster exists, so the plan shows it as "read during apply".
3. The namespace is created with that address as its only IP rule.

The namespace waits for the cluster, so it comes up a few minutes later than it could. Nothing uses it before the cluster is up anyway.

Things to know:

- Azure applies IP rules to AMQP on 5671 and HTTPS on 443, so they cover the SDK and the REST API.
- Your laptop is outside the firewall. The portal's Service Bus Explorer and any local `azservicebus` client get an unauthorised error that doesn't mention the firewall. Run checks from a pod in the cluster.
- The portal shows the Networking page only for Premium namespaces. Read the rules with `az servicebus namespace network-rule-set show`.
- The firewall can't tell pods apart. Every pod and node shares the egress IP. Entra ID roles decide who can send and receive.
- If the cluster's outbound IP changes during the day, a `tofu apply` updates the rule.
- The module refuses an empty `allowed_ip_addresses`. With Deny and no rules, the provider fails the apply.

## Access

Local auth is off, so every client needs an Entra ID role on the namespace or the queue. The platform root gives the three publishers Azure Service Bus Data Sender and the worker Azure Service Bus Data Receiver. Both are scoped to the queue through the `queue_id` output (P3-17). The role matrix is in the [workload-identity README](../workload-identity/README.md#role-matrix). Your own account has no data role, so the test below sends from a pod signed in as a publisher and receives from one signed in as the worker.

## Acceptance test

The card's test is a message abandoned three times appearing in the DLQ. It needs P3-17's roles and a pod in the cluster, since the firewall blocks everything else:

1. Send one message to `order-events`.
2. Receive it in PeekLock mode and abandon it, three times.
3. Check the queue's dead-letter count:

```bash
az servicebus queue show -g rg-aks-v2-platform --namespace-name <name> -n order-events \
  --query countDetails.deadLetterMessageCount
```

The `az` call is a management-plane read, so it works from anywhere. It should print 1.

## Outputs

| Output | Value |
| --- | --- |
| `id` | Namespace ID |
| `queue_id` | Queue ID, the scope for the Sender and Receiver roles in P3-17 |
| `name` | Namespace name |
| `fqdn` | `<name>.servicebus.windows.net`, for `SERVICEBUS_NAMESPACE` |
| `queue_name` | `order-events`, for `SERVICEBUS_QUEUE` |

No output holds a key or a connection string. With local auth off, there are none to use.

## Cost

At UK South retail prices checked on 19 November 2026:

| Item | Rate | On the schedule |
| --- | --- | --- |
| Standard base charge | £7.52 a month, billed by the hour | About £1.81 for 176 hours |
| Messaging operations | First 13 million a month free, then £0.38 per million | £0 |
| Total | | About £2 |

The base charge stops when the namespace is destroyed each evening.
