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
RELOADER_VERSION := 2.2.20

PERSISTENT_OUT := tofu -chdir=infra/persistent output
PLATFORM_OUT := tofu -chdir=infra/platform output

# The nine services, by their folder in app/services/ and k8s/apps/.
SERVICES := api-gateway order-service inventory-service payment-service notification-service \
	shipping-service dashboard-api worker scheduler

.PHONY: help init plan persistent platform kubeconfig up down cluster \
	node-pools namespaces storage external-secrets traefik cert-manager external-dns data \
	backup restore-drill reloader images pin-images apps
.PHONY: migrate

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
	$(MAKE) node-pools namespaces storage external-secrets traefik cert-manager external-dns reloader data backup
	$(MAKE) migrate
	$(MAKE) apps

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

reloader: ## Reloader: restarts a service when a Secret it names changes
	$(HELM) upgrade --install reloader reloader --repo https://stakater.github.io/stakater-charts \
	  --version $(RELOADER_VERSION) --namespace platform -f k8s/reloader/values.yaml --wait --timeout 10m

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

# Postgres starts empty every morning, so cluster runs this after data,
# to rebuild the schema. It needs only the public migrate image, no app image.
# make apps applies the Deployments after it, so a failed migration stops make
# before any rollout. ArgoCD runs the same Job as a PreSync hook from P7.
# See k8s/migrations/README.md.
# The old Job goes first: a finished Job of the same name would otherwise
# leave apply unchanged and report success without migrating, for example
# after the database was reset.
migrate: ## Run pending DB migrations as a Job and wait. Fails if the Job fails
	set -euo pipefail; \
	$(KUBECTL) -n data rollout status statefulset/postgres --timeout=1m >/dev/null \
	  || { echo "Postgres is not ready in data. Run make data first." >&2; exit 1; }; \
	$(KUBECTL) -n apps delete job db-migrate --ignore-not-found --cascade=foreground --wait; \
	$(KUBECTL) apply -k k8s/migrations; \
	echo "Waiting for job/db-migrate"; \
	for i in $$(seq 1 70); do \
	  STATE=$$($(KUBECTL) -n apps get job db-migrate -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type} {end}') || STATE=; \
	  case "$$STATE" in \
	    *Complete*) \
	      POD=$$($(KUBECTL) -n apps get pods -l job-name=db-migrate --field-selector=status.phase=Succeeded -o name) || POD=; \
	      [[ -z "$$POD" ]] || $(KUBECTL) -n apps logs "$$POD" --tail=20 || true; \
	      exit 0 ;; \
	    *Failed*) \
	      $(KUBECTL) -n apps logs job/db-migrate --tail=20 >&2 || true; \
	      echo "job/db-migrate failed. Do not roll out the apps: see k8s/migrations/README.md." >&2; \
	      exit 1 ;; \
	  esac; \
	  sleep 5; \
	done; \
	echo "job/db-migrate still running after 350s." >&2; exit 1

# Each image is tagged with the first 12 characters of the last commit that
# touched its folder, so the tag names exactly what was built and the dev
# overlays can pin it in Git. A tag already in ACR is never rebuilt, so tags
# stay immutable. Uncommitted changes in a service folder stop the build:
# the tag would name a commit that doesn't hold them. See k8s/apps/README.md.
images: ## Build each service into ACR with az acr build. Skips tags already there
	set -euo pipefail; \
	ACR=$$($(PERSISTENT_OUT) -raw acr_name) \
	  || { echo "No acr_name in the persistent outputs. Apply persistent first." >&2; exit 1; }; \
	[[ "$$ACR" =~ ^[a-z0-9]{5,50}$$ ]] || { echo "ACR name '$$ACR' is not a registry name." >&2; exit 1; }; \
	for svc in $(SERVICES); do \
	  DIR=app/services/$$svc; \
	  if ! git diff --quiet HEAD -- "$$DIR" || [[ -n "$$(git ls-files --others --exclude-standard -- "$$DIR")" ]]; then \
	    echo "$$DIR has uncommitted changes. Commit them first, so the tag names what is built." >&2; exit 1; \
	  fi; \
	  TAG=$$(git log -1 --format=%H -- "$$DIR" | cut -c1-12); \
	  [[ "$$TAG" =~ ^[0-9a-f]{12}$$ ]] || { echo "No commit touches $$DIR." >&2; exit 1; }; \
	  if az acr repository show --name "$$ACR" --image "$$svc:$$TAG" >/dev/null 2>&1; then \
	    echo "$$svc:$$TAG is already in $$ACR"; continue; \
	  fi; \
	  az acr build --registry "$$ACR" --image "$$svc:$$TAG" --platform linux/amd64 "$$DIR"; \
	done

# Offline. Sets newTag in each dev overlay to the tag make images builds,
# with sed so the overlay's comments survive (kustomize edit drops them).
# Commit the result.
pin-images: ## Pin each dev overlay to its service's current image tag. Edits Git
	set -euo pipefail; \
	for svc in $(SERVICES); do \
	  TAG=$$(git log -1 --format=%H -- "app/services/$$svc" | cut -c1-12); \
	  [[ "$$TAG" =~ ^[0-9a-f]{12}$$ ]] || { echo "No commit touches app/services/$$svc." >&2; exit 1; }; \
	  F=k8s/apps/$$svc/overlays/dev/kustomization.yaml; \
	  sed -E "s/^(    newTag: ).*/\1$$TAG/" "$$F" > "$$F.tmp"; \
	  mv "$$F.tmp" "$$F"; \
	  echo "$$svc: $$TAG"; \
	done

# The services go in after the migration, so a failed migration stops make
# before any rollout. azure.env carries the values that stay out of Git:
# the registry, the Service Bus namespace and queue, and the workload
# identities' client IDs. Every value is read and checked before anything
# is written. See k8s/apps/README.md.
apps: ## The nine services in apps. IDs come from tofu output. Waits for every rollout
	set -euo pipefail; \
	IDS=$$($(PERSISTENT_OUT) -json workload_identity_client_ids) \
	  || { echo "No workload_identity_client_ids in the persistent outputs. Apply persistent first." >&2; exit 1; }; \
	ACR=$$($(PERSISTENT_OUT) -raw acr_login_server) \
	  || { echo "No acr_login_server in the persistent outputs. Apply persistent first." >&2; exit 1; }; \
	SB=$$($(PLATFORM_OUT) -raw servicebus_fqdn) \
	  || { echo "No servicebus_fqdn in the platform outputs. Apply platform first." >&2; exit 1; }; \
	QUEUE=$$($(PLATFORM_OUT) -raw servicebus_queue_name) \
	  || { echo "No servicebus_queue_name in the platform outputs. Apply platform first." >&2; exit 1; }; \
	[[ "$$ACR" =~ ^[a-z0-9]+\.azurecr\.io$$ ]] || { echo "Registry '$$ACR' is not an ACR login server." >&2; exit 1; }; \
	[[ "$$SB" =~ ^[a-z0-9-]+\.servicebus\.windows\.net$$ ]] || { echo "'$$SB' is not a Service Bus namespace FQDN." >&2; exit 1; }; \
	[[ "$$QUEUE" =~ ^[A-Za-z0-9._-]+$$ ]] || { echo "'$$QUEUE' is not a queue name." >&2; exit 1; }; \
	ENV="acrLoginServer=$$ACR"$$'\n'"serviceBusNamespace=$$SB"$$'\n'"serviceBusQueue=$$QUEUE"; \
	for id in order-service payment-service shipping-service worker; do \
	  CLIENT_ID=$$(jq -er --arg k "$$id" '.[$$k]' <<<"$$IDS") \
	    || { echo "No $$id client ID in the persistent outputs." >&2; exit 1; }; \
	  [[ "$$CLIENT_ID" =~ ^[0-9a-f-]{36}$$ ]] || { echo "Client ID '$$CLIENT_ID' for $$id is not a GUID." >&2; exit 1; }; \
	  ENV="$$ENV"$$'\n'"$$id=$$CLIENT_ID"; \
	done; \
	printf '%s\n' "$$ENV" > k8s/apps/dev/azure.env; \
	$(KUBECTL) apply -k k8s/apps/dev; \
	$(KUBECTL) -n apps wait externalsecret --all --for=condition=Ready --timeout=2m; \
	for svc in $(SERVICES); do \
	  $(KUBECTL) -n apps rollout status deployment/$$svc --timeout=10m; \
	done
