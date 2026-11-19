# network

**TL;DR:** The platform VNet, a node subnet and a private endpoint subnet, each with its own NSG. Neither subnet has default outbound access, and no node gets a public IP. The address plan is below. All of it is free.

The `platform` root calls this module, so the network goes with the rest of the daily stack every evening. Filled by P3-04 (#30).

## Address plan

| Range | CIDR | Purpose | Capacity |
| --- | --- | --- | --- |
| VNet | `10.20.0.0/16` | The whole platform network | 65,536 addresses |
| `snet-aks-nodes` | `10.20.0.0/22` | AKS node NICs. Internal load balancer frontends would also go here | 1,019 usable IPs (Azure keeps 5 per subnet) |
| `snet-private-endpoints` | `10.20.4.0/24` | Private endpoints for ACR, Key Vault, Service Bus and blob (P3-14) | 251 usable IPs |
| Reserved | `10.20.5.0` to `10.20.255.255` | Later subnets, such as API server VNet integration (a /28 or larger) or a jumpbox. Not created | About 64,000 addresses |
| Pod CIDR | `10.244.0.0/16` | Azure CNI Overlay pod IPs. Outside the VNet. Not routable from it | 256 nodes at the default /24 per node, 250 pods each |
| Service CIDR | `10.0.0.0/16` | Kubernetes ClusterIP services. Outside the VNet. P3-07 sets it | 65,536 service IPs |
| DNS service IP | `10.0.0.10` | CoreDNS ClusterIP, inside the service CIDR. P3-07 sets it | One address |

The VNet, pod CIDR and service CIDR must not overlap. They don't. The pod and service CIDRs are the AKS defaults.

### Why the node subnet is a /22

With Overlay, a node takes one subnet IP and its pods take IPs from the pod CIDR. The subnet only has to hold nodes. A system pool of three nodes, a user pool, Node Auto Provisioning growth and surge nodes during upgrades come to tens of nodes at most. A /24 would do. A /22 costs nothing and gives room for internal load balancer frontends and for an upgrade that surges every pool at once. Growing a subnet in use is awkward, so it starts large.

## No load balancer subnet

The card lists a load balancer subnet. This module doesn't create one, for these reasons:

- The planned load balancer is public. AKS egress (P3-05) uses the outbound rules of the cluster's Standard Load Balancer, and Traefik (P4-03) sits behind the same kind of load balancer with a public IP. A public Standard Load Balancer has no subnet. Its frontend is a public IP, and its backend pool is the node NICs in `snet-aks-nodes`.
- An internal load balancer would need a frontend IP in a subnet. AKS puts it in the node subnet unless a Service sets the `service.beta.kubernetes.io/azure-load-balancer-internal-subnet` annotation. Nothing in the plan needs an internal one.
- An empty subnet would cost nothing but would suggest a design that doesn't exist.

If an internal load balancer is needed later, take a /24 from the reserved range and point the annotation at it.

## Default outbound access

Both subnets set `default_outbound_access_enabled = false`, so a VM in them gets no implicit internet access from Azure. This doesn't break P3-05. With outbound type `loadBalancer`, AKS gives the Standard Load Balancer outbound rules and a public IP, and node egress uses those. Explicit outbound methods like that one, or a NAT Gateway, still work on a private subnet.

## NSG rules

Every NSG starts with the Azure default rules. Inbound, they allow traffic within the VNet (`AllowVnetInBound`, 65000) and from `AzureLoadBalancer` (`AllowAzureLoadBalancerInBound`, 65001), then deny the rest. Outbound, they allow the VNet and the internet. The tables list only the rules this module adds.

AKS also manages an NSG of its own in the node resource group, attached to the node NICs. It adds rules there for each load balancer Service. Traffic must pass both NSGs, so the subnet NSG must not block what AKS opens.

### nsg-aks-nodes

| Priority | Name | Source | Destination | Ports | Why |
| --- | --- | --- | --- | --- | --- |
| 100 | `AllowPodCidrInbound` | Pod CIDR | Node subnet and pod CIDR | Any | Overlay pod traffic between nodes is not encapsulated, so the NSG sees pod IPs. The pod CIDR is outside the VNet, so `AllowVnetInBound` may not match it. Microsoft's Overlay docs list this rule as required |
| 110 | `AllowInternetHttpHttpsInbound` | `Internet` | Any | TCP 80, 443 | Traefik's public load balancer (P4-03). The default deny would block it. The nodes have no public IPs, so only a load balancer frontend with a rule for these ports is reachable. The destination is `*` because with floating IP the NSG sees the frontend IP, which doesn't exist until the cluster creates it |

Not added, and why:

- Traffic within the VNet and load balancer health probes. The defaults allow both.
- Outbound limits. AKS needs to reach Microsoft endpoints for images, the API server and updates. A tight outbound list belongs with a firewall, not here.

### nsg-private-endpoints

The subnet sets `private_endpoint_network_policies = "NetworkSecurityGroupEnabled"`. Without it, Azure ignores the NSG for private endpoint traffic. There is no route table, so the route table part stays off.

| Priority | Name | Source | Destination | Ports | Why |
| --- | --- | --- | --- | --- | --- |
| 100 | `AllowVnetServicesInbound` | `VirtualNetwork` | Endpoint subnet | TCP 443, 5671 | HTTPS for ACR, Key Vault, blob and Service Bus. 5671 is AMQP over TLS for Service Bus. Pod traffic leaving the cluster is SNATed to the node IP, so the `VirtualNetwork` tag covers pods |
| 4096 | `DenyAllInbound` | Any | Any | Any | Overrides the defaults that allow any port from the VNet and from `AzureLoadBalancer`. Private endpoints need neither |

A private endpoint for a service on another port, such as SQL on 1433, needs that port added to rule 100.

## Outputs

| Output | Value |
| --- | --- |
| `vnet_id` | ID of the VNet |
| `vnet_name` | Name of the VNet, for the private DNS zone links in P3-06 |
| `subnet_ids` | Map with `aks_nodes` and `private_endpoints` |

## Cost

A VNet, its subnets and NSGs are free. Charges start with what goes in them: nodes, load balancer rules, public IPs, private endpoints and data transfer.
