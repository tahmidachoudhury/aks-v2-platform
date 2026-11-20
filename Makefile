# Daily routine for the sandbox: OpenTofu roots and the in-cluster platform
# components, in dependency order. ArgoCD takes over the in-cluster part in
# P7; until then this is the one place the install commands and chart
# versions live.
#
#   make up      morning: platform apply, kubeconfig, then every component
#   make down    evening: platform destroy
#   make help    every target
#
# Works with the GNU Make 3.81 macOS ships: multi-line recipes are joined
# with backslashes, since 3.81 has no .ONESHELL, and recipes that read
# values start with set -euo pipefail, since 3.81 ignores .SHELLFLAGS.
#
# tofu apply and destroy stay interactive: each shows its plan and waits for
# "yes". Nothing here passes -auto-approve.

SHELL := /bin/bash
.DEFAULT_GOAL := help
# Order matters: namespaces before the Helm installs, Traefik before
# cert-manager. Never run targets in parallel, even under make -j.
.NOTPARALLEL:

# The context az aks get-credentials creates, named after the cluster.
CTX := aks-v2-platform
KUBECTL := kubectl --context $(CTX)
HELM := helm --kube-context $(CTX)

# A repo config of our own, so a stale entry in the operator's Helm config
# can't break the --repo installs. Gitignored.
export HELM_REPOSITORY_CONFIG := $(CURDIR)/.helm/repositories.yaml
export HELM_REPOSITORY_CACHE := $(CURDIR)/.helm/cache

ESO_VERSION := 2.15.0
TRAEFIK_VERSION := 42.4.1
CERT_MANAGER_VERSION := v1.21.3
EXTERNAL_DNS_VERSION := 1.23.0

PERSISTENT_OUT := tofu -chdir=infra/persistent output

.PHONY: help init plan persistent platform kubeconfig up down cluster \
	node-pools namespaces storage external-secrets traefik cert-manager external-dns data \
	backup restore-drill

help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-17s %s\n", $$1, $$2}'

init: ## tofu init the persistent and platform roots
	for root in persistent platform; do \
	  tofu -chdir=infra/$$root init -input=false -backend-config=../backend.hcl; \
	done

plan: ## Plan persistent, then platform
	tofu -chdir=infra/persistent plan
	tofu -chdir=infra/platform plan

persistent: ## Apply persistent. Rarely needed, never destroyed
	tofu -chdir=infra/persistent apply

platform: ## Apply platform: network, AKS, Service Bus, endpoints
	tofu -chdir=infra/platform apply

kubeconfig: ## Fetch an Entra ID kubeconfig for the cluster
	eval "$$(tofu -chdir=infra/platform output -raw kubeconfig_command)"

up: ## Morning: platform, kubeconfig, then every component
	$(MAKE) platform
	$(MAKE) kubeconfig
	$(MAKE) cluster

cluster: ## Install every in-cluster component, in order
	$(MAKE) node-pools namespaces storage external-secrets traefik cert-manager external-dns data backup

down: ## Evening: destroy platform. The cluster and everything in it go with it
	tofu -chdir=infra/platform destroy

node-pools: ## NAP NodePools, so workloads have somewhere to run
	$(KUBECTL) apply -k k8s/node-pools

namespaces: ## platform, apps and data, with quotas and Pod Security
	$(KUBECTL) apply -k k8s/namespaces

storage: ## Default StorageClass and VolumeSnapshotClass
	$(KUBECTL) apply -k k8s/storage

external-secrets: ## ESO and the ClusterSecretStore. IDs come from tofu output
	set -euo pipefail; \
	CLIENT_ID=$$($(PERSISTENT_OUT) -json workload_identity_client_ids | jq -er '."external-secrets"') \
	  || { echo "No external-secrets client ID in the persistent outputs. Apply persistent first." >&2; exit 1; }; \
	VAULT_URL=$$($(PERSISTENT_OUT) -raw key_vault_uri) \
	  || { echo "No key_vault_uri in the persistent outputs. Apply persistent first." >&2; exit 1; }; \
	[[ "$$CLIENT_ID" =~ ^[0-9a-f-]{36}$$ ]] || { echo "Client ID '$$CLIENT_ID' is not a GUID." >&2; exit 1; }; \
	[[ "$$VAULT_URL" == https://*.vault.azure.net/ ]] || { echo "Vault URL '$$VAULT_URL' is not a Key Vault URI." >&2; exit 1; }; \
	echo "vaultUrl=$$VAULT_URL" > k8s/external-secrets/store/vault.env; \
	$(HELM) upgrade --install external-secrets oci://ghcr.io/external-secrets/charts/external-secrets \
	  --version $(ESO_VERSION) --namespace platform -f k8s/external-secrets/values.yaml \
	  --set-string serviceAccount.annotations."azure\.workload\.identity/client-id"="$$CLIENT_ID" \
	  --wait --timeout 10m; \
	$(KUBECTL) apply -k k8s/external-secrets/store

traefik: ## Traefik on the persistent ingress IP
	$(HELM) upgrade --install traefik traefik --repo https://traefik.github.io/charts \
	  --version $(TRAEFIK_VERSION) --namespace platform -f k8s/traefik/values.yaml --wait --timeout 10m

cert-manager: ## cert-manager and the Let's Encrypt ClusterIssuers
	$(HELM) upgrade --install cert-manager cert-manager --repo https://charts.jetstack.io \
	  --version $(CERT_MANAGER_VERSION) --namespace platform -f k8s/cert-manager/values.yaml --wait --timeout 10m
	$(KUBECTL) apply -k k8s/cert-manager

external-dns: ## ExternalDNS for the public zone. IDs come from tofu output and persistent's tfvars
	set -euo pipefail; \
	SUB=$$(awk -F'"' '/^[[:space:]]*subscription_id[[:space:]]*=/ {print $$2; exit}' infra/persistent/terraform.tfvars); \
	CLIENT_ID=$$($(PERSISTENT_OUT) -json workload_identity_client_ids | jq -er '."external-dns"') \
	  || { echo "No external-dns client ID in the persistent outputs. Apply persistent first." >&2; exit 1; }; \
	ZONE=$$($(PERSISTENT_OUT) -raw dns_zone_name) \
	  || { echo "No dns_zone_name in the persistent outputs." >&2; exit 1; }; \
	ZONE_RG=$$($(PERSISTENT_OUT) -raw resource_group_name) \
	  || { echo "No resource_group_name in the persistent outputs." >&2; exit 1; }; \
	[[ "$$SUB" =~ ^[0-9a-f-]{36}$$ ]] || { echo "No subscription_id in infra/persistent/terraform.tfvars, the root that holds the zone." >&2; exit 1; }; \
	[[ "$$CLIENT_ID" =~ ^[0-9a-f-]{36}$$ ]] || { echo "Client ID '$$CLIENT_ID' is not a GUID." >&2; exit 1; }; \
	[[ -n "$$ZONE" && -n "$$ZONE_RG" ]] || { echo "No dns_zone_name or resource_group_name output." >&2; exit 1; }; \
	$(HELM) upgrade --install external-dns external-dns --repo https://kubernetes-sigs.github.io/external-dns/ \
	  --version $(EXTERNAL_DNS_VERSION) --namespace platform -f k8s/external-dns/values.yaml \
	  --set-string serviceAccount.annotations."azure\.workload\.identity/client-id"="$$CLIENT_ID" \
	  --set-string extraArgs.azure-subscription-id="$$SUB" \
	  --set-string extraArgs.azure-resource-group="$$ZONE_RG" \
	  --set-string "domainFilters[0]=$$ZONE" \
	  --wait --timeout 10m

data: ## Postgres and Redis in data. Waits for both pods, then (re)applies the app role
	$(KUBECTL) apply -k k8s/data
	$(KUBECTL) -n data wait externalsecret --all --for=condition=Ready --timeout=2m
	$(KUBECTL) -n data rollout status statefulset/postgres --timeout=10m
	$(KUBECTL) -n data rollout status statefulset/redis --timeout=10m
	$(KUBECTL) -n data exec -i postgres-0 -- psql -q -v ON_ERROR_STOP=1 -U postgres -d orders < k8s/data/postgres/app-role.sql

backup: ## Hourly Postgres snapshot CronJob in data, keeping six. Needs make data first
	$(KUBECTL) -n data get pvc data-postgres-0 >/dev/null \
	  || { echo "No PVC data-postgres-0 in data. Run make data before make backup." >&2; exit 1; }
	$(KUBECTL) apply -k k8s/backup

restore-drill: ## Snapshot Postgres, restore it to a temporary pod, check the rows, clean up
	CONTEXT=$(CTX) k8s/backup/restore-drill.sh
