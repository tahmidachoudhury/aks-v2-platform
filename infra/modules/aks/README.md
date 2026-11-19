# aks

**TL;DR:** Will hold the AKS cluster. Empty until P3-07 (#33) fills it.

The `platform` root will call it. Node Auto Provisioning (P3-08, #34) and the add-ons (P3-09, #35) extend it later. AKS creates its own node resource group; this module does not manage that group.
