# dns

**TL;DR:** Will hold the public Azure DNS zone for the domain. Empty until P3-15 (#41) fills it.

The `persistent` root will call it. Recreating the zone would assign new name servers and break the delegation at the registrar. The private DNS zones for private endpoints (P3-06, #32) belong to `platform`; P3-06 decides whether they go in this module or in `network`.
