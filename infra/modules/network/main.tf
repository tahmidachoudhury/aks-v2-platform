# Address plan and the reason for every rule: see README.md.

resource "azurerm_virtual_network" "this" {
  name                = var.name
  resource_group_name = var.resource_group_name
  location            = var.location
  address_space       = [var.address_space]
  tags                = var.tags
}

# One IP per node. With Azure CNI Overlay, pods take their IPs from the pod
# CIDR, not from this subnet.
resource "azurerm_subnet" "aks_nodes" {
  name                 = "snet-aks-nodes"
  resource_group_name  = var.resource_group_name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = [var.aks_nodes_prefix]

  # No implicit internet egress. AKS egress comes from the outbound rules of
  # its Standard Load Balancer (P3-05).
  default_outbound_access_enabled = false
}

resource "azurerm_subnet" "private_endpoints" {
  name                 = "snet-private-endpoints"
  resource_group_name  = var.resource_group_name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = [var.private_endpoints_prefix]

  # Apply the subnet NSG to private endpoint traffic. Without this, Azure
  # skips NSG rules for private endpoints. No route table here, so only the
  # NSG part is enabled.
  private_endpoint_network_policies = "NetworkSecurityGroupEnabled"
}

# AKS nodes. The default rules already allow traffic within the VNet and
# from AzureLoadBalancer, and deny the rest. The rules below add only what
# those defaults miss.
resource "azurerm_network_security_group" "aks_nodes" {
  name                = "nsg-aks-nodes"
  resource_group_name = var.resource_group_name
  location            = var.location
  tags                = var.tags
}

# Overlay pod traffic between nodes is not encapsulated, so the NSG sees pod
# IPs. The pod CIDR is outside the VNet, so AllowVnetInBound does not cover
# it. Microsoft's Overlay guidance needs node to pod and pod to pod allowed.
resource "azurerm_network_security_rule" "aks_nodes_allow_pod_cidr" {
  name                         = "AllowPodCidrInbound"
  priority                     = 100
  direction                    = "Inbound"
  access                       = "Allow"
  protocol                     = "*"
  source_port_range            = "*"
  destination_port_range       = "*"
  source_address_prefixes      = [var.aks_nodes_prefix, var.pod_cidr]
  destination_address_prefixes = [var.aks_nodes_prefix, var.pod_cidr]
  resource_group_name          = var.resource_group_name
  network_security_group_name  = azurerm_network_security_group.aks_nodes.name
}

# Ingress through the public Standard Load Balancer (Traefik, P4-03). The
# nodes have no public IPs, so only a load balancer frontend with a rule for
# these ports is reachable. This is a pass-through for any public
# LoadBalancer Service on 80 or 443, not just Traefik; the AKS-managed NSG
# on the node NICs is the per-service gate. The destination stays "*" because the address the
# NSG sees is the frontend IP when floating IP is on, and that IP does not
# exist until the cluster creates it.
resource "azurerm_network_security_rule" "aks_nodes_allow_http_https" {
  name                        = "AllowInternetHttpHttpsInbound"
  priority                    = 110
  direction                   = "Inbound"
  access                      = "Allow"
  protocol                    = "Tcp"
  source_port_range           = "*"
  destination_port_ranges     = ["80", "443"]
  source_address_prefix       = "Internet"
  destination_address_prefix  = "*"
  resource_group_name         = var.resource_group_name
  network_security_group_name = azurerm_network_security_group.aks_nodes.name
}

resource "azurerm_subnet_network_security_group_association" "aks_nodes" {
  subnet_id                 = azurerm_subnet.aks_nodes.id
  network_security_group_id = azurerm_network_security_group.aks_nodes.id
}

# Private endpoints accept traffic from the VNet only, on the ports the
# planned services use.
resource "azurerm_network_security_group" "private_endpoints" {
  name                = "nsg-private-endpoints"
  resource_group_name = var.resource_group_name
  location            = var.location
  tags                = var.tags
}

# HTTPS for Key Vault and blob. ACR Basic and Service Bus Standard have no
# private endpoint (see docs/architecture-decisions.md). Pod traffic to the VNet is SNATed to the node IP, so the
# VirtualNetwork tag covers it.
resource "azurerm_network_security_rule" "private_endpoints_allow_vnet" {
  name                        = "AllowVnetServicesInbound"
  priority                    = 100
  direction                   = "Inbound"
  access                      = "Allow"
  protocol                    = "Tcp"
  source_port_range           = "*"
  destination_port_range      = "443"
  source_address_prefix       = "VirtualNetwork"
  destination_address_prefix  = var.private_endpoints_prefix
  resource_group_name         = var.resource_group_name
  network_security_group_name = azurerm_network_security_group.private_endpoints.name
}

# Overrides the default AllowVnetInBound (any port) and
# AllowAzureLoadBalancerInBound. Private endpoints need neither.
resource "azurerm_network_security_rule" "private_endpoints_deny_all" {
  name                        = "DenyAllInbound"
  priority                    = 4096
  direction                   = "Inbound"
  access                      = "Deny"
  protocol                    = "*"
  source_port_range           = "*"
  destination_port_range      = "*"
  source_address_prefix       = "*"
  destination_address_prefix  = "*"
  resource_group_name         = var.resource_group_name
  network_security_group_name = azurerm_network_security_group.private_endpoints.name
}

resource "azurerm_subnet_network_security_group_association" "private_endpoints" {
  subnet_id                 = azurerm_subnet.private_endpoints.id
  network_security_group_id = azurerm_network_security_group.private_endpoints.id
}

# Private DNS zones for private endpoints (P3-14). A private endpoint gets a
# private IP in snet-private-endpoints. The zone maps the service's public
# name to that IP for anything resolving through Azure DNS from this VNet.
# The caller picks the zones: see README.md for which ones and why.
resource "azurerm_private_dns_zone" "this" {
  for_each = var.private_dns_zones

  name                = each.value
  resource_group_name = var.resource_group_name
  tags                = var.tags
}

# Registration stays off: these zones hold only the A records the private
# endpoints write, not records for VMs in the VNet.
resource "azurerm_private_dns_zone_virtual_network_link" "this" {
  for_each = var.private_dns_zones

  name                  = "${var.name}-link"
  resource_group_name   = var.resource_group_name
  private_dns_zone_name = azurerm_private_dns_zone.this[each.key].name
  virtual_network_id    = azurerm_virtual_network.this.id
  registration_enabled  = false

  # Fall back to public DNS when a name has no record here. Without this, a
  # storage account or vault with a private endpoint in some other VNet
  # resolves to NXDOMAIN from the cluster.
  resolution_policy = "NxDomainRedirect"

  tags = var.tags
}
