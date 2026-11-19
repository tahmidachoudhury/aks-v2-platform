# dns

**TL;DR:** The public Azure DNS zone `azure.order-fulfilment.tahmidchoudhury.uk`, about $0.50 a month. Filled by P3-15 (#41) and called from the `persistent` root. After the first apply, delegate it from the parent zone by hand with four NS records.

## What it creates

One `azurerm_dns_zone`. The name comes from the root's `dns_zone_name` variable, which defaults to `azure.order-fulfilment.tahmidchoudhury.uk`.

`prevent_destroy` is set. A recreated zone gets new name servers, and the delegation in the parent zone would point at the old ones.

ExternalDNS (P5) writes the app's records into this zone. The private DNS zones for private endpoints (P3-06, #32) belong to `platform`, not here.

## Cost

About $0.50 a month for the zone, plus $0.40 per million queries. A learning build sends far fewer than a million.

## Inputs

| Name | Notes |
| --- | --- |
| `name` | The zone name |
| `resource_group_name` | |
| `tags` | |

## Outputs

`id`, `name` and `name_servers`.

## Delegate the zone

Do this once, by hand, after the first apply of `persistent`. The parent zone is `tahmidchoudhury.uk`. Change it wherever that zone is hosted: the registrar's DNS panel, or an Azure DNS zone if it lives in Azure.

1. Get the four name servers:

   ```bash
   tofu -chdir=infra/persistent output dns_zone_name_servers
   ```

2. In the parent zone, add four NS records with the host name `azure.order-fulfilment`, one for each name server. Keep the trailing dot if the panel asks for a fully qualified name. If the parent zone is in Azure DNS, one record set holds all four:

   ```bash
   az network dns record-set ns add-record -g <parent rg> -z tahmidchoudhury.uk \
     -n azure.order-fulfilment -d <name server>
   ```

   Run it once per name server.

3. Check the delegation. This should print the four Azure name servers:

   ```bash
   dig NS azure.order-fulfilment.tahmidchoudhury.uk +short
   ```

   An empty answer means the NS records are missing or not yet live. Registrars can take up to an hour.

The name servers do not change while the zone exists, so this is a one-off.
