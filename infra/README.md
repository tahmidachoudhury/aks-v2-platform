# infra

Terraform for the Azure side of the platform: resource group, VNet and subnets, AKS, ACR, Key Vault,
Service Bus, DNS, private endpoints, and the storage account that holds remote state.

Reusable pieces go in `modules/`. Each deployable root (for example the state bootstrap and the main
environment) gets its own folder, so you can plan and apply each one separately.
