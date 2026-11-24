# Daily routine for the sandbox: the OpenTofu roots, in dependency order.
# Argo CD installs everything in the cluster from Git: the argocd root puts
# in Argo CD and its root Application, and the root brings up the rest (see
# argocd/README.md).
#
#   make up      morning: platform apply, kubeconfig, then the argocd apply
#   make down    evening: argocd destroy, then platform destroy
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
# Order matters in up and down. Never run targets in parallel, even under
# make -j.
.NOTPARALLEL:

# The context az aks get-credentials creates, named after the cluster.
CTX := aks-v2-platform
KUBECTL := kubectl --context $(CTX)

PERSISTENT_OUT := tofu -chdir=infra/persistent output

# make loadtest settings: how long (whole minutes, under the Job's one-hour
# deadline), how many page loads and logins a second each of its three pods
# sends, and an optional address to send the host's traffic to instead of
# its public one. Override on the command line.
DURATION ?= 5m
RATE ?= 10
TARGET_IP ?=

# The nine services, by their folder in app/services/ and k8s/apps/.
SERVICES := api-gateway order-service inventory-service payment-service notification-service \
	shipping-service dashboard-api worker scheduler

.PHONY: help init plan persistent platform argocd kubeconfig up down \
	restore-drill images pin-images loadtest

help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-17s %s\n", $$1, $$2}'

init: ## tofu init the persistent, platform and argocd roots
	for root in persistent platform argocd; do \
	  tofu -chdir=infra/$$root init -input=false -backend-config=../backend.hcl; \
	done

# argocd reads the cluster, so its plan needs platform applied.
plan: ## Plan persistent, platform, then argocd
	tofu -chdir=infra/persistent plan
	tofu -chdir=infra/platform plan
	tofu -chdir=infra/argocd plan

persistent: ## Apply persistent. Rarely needed, never destroyed
	tofu -chdir=infra/persistent apply

platform: ## Apply platform: network, AKS, Service Bus, endpoints
	tofu -chdir=infra/platform apply

argocd: ## Apply argocd: Argo CD and the root Application, which installs the rest
	tofu -chdir=infra/argocd apply

kubeconfig: ## Fetch an Entra ID kubeconfig for the cluster
	eval "$$(tofu -chdir=infra/platform output -raw kubeconfig_command)"

up: ## Morning: platform, kubeconfig, then argocd
	$(MAKE) platform
	$(MAKE) kubeconfig
	$(MAKE) argocd

# argocd first, so its Helm releases go while the cluster still exists. The
# Applications have no finalizer, so nothing waits on a cascade.
down: ## Evening: destroy argocd, then platform. The cluster goes, with everything in it
	tofu -chdir=infra/argocd destroy
	tofu -chdir=infra/platform destroy

restore-drill: ## Snapshot Postgres, restore it to a temporary pod, check the rows, clean up
	CONTEXT=$(CTX) k8s/backup/restore-drill.sh

# Each image is tagged with the first 12 characters of the last commit that
# touched its folder, so the tag names exactly what was built and the dev
# overlays can pin it in Git. Tags are immutable: a tag already in ACR is
# never rebuilt, and each tag is locked with write-enabled false, so a later
# push or delete of it fails. Whether a tag exists comes from listing the
# repositories and the service's tags, calls that succeed when nothing is
# there, so any az error stops make rather than starting a build.
# Uncommitted changes in a service folder stop the build too: the tag would
# name a commit that doesn't hold them. Every az call names the persistent
# root's subscription, which holds the registry, so the az default doesn't
# matter. See k8s/apps/README.md.
images: ## Build each service into ACR with az acr build. Skips tags already there
	set -euo pipefail; \
	ACR=$$($(PERSISTENT_OUT) -raw acr_name) \
	  || { echo "No acr_name in the persistent outputs. Apply persistent first." >&2; exit 1; }; \
	[[ "$$ACR" =~ ^[a-z0-9]{5,50}$$ ]] || { echo "ACR name '$$ACR' is not a registry name." >&2; exit 1; }; \
	SUB=$$(awk -F'"' '/^[[:space:]]*subscription_id[[:space:]]*=/ {print $$2; exit}' infra/persistent/terraform.tfvars); \
	[[ "$$SUB" =~ ^[0-9a-f-]{36}$$ ]] || { echo "No subscription_id in infra/persistent/terraform.tfvars, the root that holds the registry." >&2; exit 1; }; \
	REPOS=$$(az acr repository list --subscription "$$SUB" --name "$$ACR" -o tsv) \
	  || { echo "Could not list the repositories in $$ACR." >&2; exit 1; }; \
	for svc in $(SERVICES); do \
	  DIR=app/services/$$svc; \
	  if ! git diff --quiet HEAD -- "$$DIR" || [[ -n "$$(git ls-files --others --exclude-standard -- "$$DIR")" ]]; then \
	    echo "$$DIR has uncommitted changes. Commit them first, so the tag names what is built." >&2; exit 1; \
	  fi; \
	  TAG=$$(git log -1 --format=%H -- "$$DIR" | cut -c1-12); \
	  [[ "$$TAG" =~ ^[0-9a-f]{12}$$ ]] || { echo "No commit touches $$DIR." >&2; exit 1; }; \
	  TAGS=; \
	  if grep -qx "$$svc" <<<"$$REPOS"; then \
	    TAGS=$$(az acr repository show-tags --subscription "$$SUB" --name "$$ACR" --repository "$$svc" -o tsv) \
	      || { echo "Could not list the tags of $$svc in $$ACR." >&2; exit 1; }; \
	  fi; \
	  if grep -qx "$$TAG" <<<"$$TAGS"; then \
	    echo "$$svc:$$TAG is already in $$ACR"; \
	  else \
	    az acr build --subscription "$$SUB" --registry "$$ACR" --image "$$svc:$$TAG" --platform linux/amd64 "$$DIR"; \
	  fi; \
	  az acr repository update --subscription "$$SUB" --name "$$ACR" --image "$$svc:$$TAG" --write-enabled false >/dev/null; \
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
	  grep -qx "    newTag: $$TAG" "$$F" || { echo "$$F has no newTag line to pin. Check its images entry." >&2; exit 1; }; \
	  echo "$$svc: $$TAG"; \
	done

# Not part of up: a drill, like restore-drill. Three k6 pods in
# default send page loads, logins and one order a second each to the public
# host, then exit. The Job fails if any request fails. The old Job goes
# first, so every run starts clean. See k8s/loadtest/README.md.
loadtest: ## k6 load test against the public host. DURATION=5m RATE=10 TARGET_IP= by default
	set -euo pipefail; \
	[[ "$(DURATION)" =~ ^([1-9]|[1-4][0-9]|5[0-5])m$$ ]] || { echo "DURATION '$(DURATION)' must be 1m to 55m: the Job stops at one hour, pod start included." >&2; exit 1; }; \
	[[ "$(RATE)" =~ ^[1-9][0-9]*$$ ]] || { echo "RATE '$(RATE)' must be a whole number from 1." >&2; exit 1; }; \
	[[ -z "$(TARGET_IP)" || "$(TARGET_IP)" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$$ ]] || { echo "TARGET_IP '$(TARGET_IP)' is not an IPv4 address." >&2; exit 1; }; \
	$(KUBECTL) -n default delete job loadtest --ignore-not-found --cascade=foreground --wait; \
	$(KUBECTL) -n default create configmap loadtest-settings \
	  --from-literal=DURATION=$(DURATION) --from-literal=RATE=$(RATE) --from-literal=TARGET_IP=$(TARGET_IP) \
	  --dry-run=client -o yaml | $(KUBECTL) apply -f -; \
	$(KUBECTL) apply -k k8s/loadtest; \
	echo "Waiting for job/loadtest ($(DURATION) of load)"; \
	STATE=; \
	for i in $$(seq 1 390); do \
	  STATE=$$($(KUBECTL) -n default get job loadtest --ignore-not-found \
	    -o jsonpath='{.metadata.name} {range .status.conditions[?(@.status=="True")]}{.type} {end}') || STATE=unknown; \
	  [[ -n "$$STATE" ]] || { echo "job/loadtest is gone before it finished. Something deleted it: run make loadtest again." >&2; exit 1; }; \
	  [[ "$$STATE" != *Complete* && "$$STATE" != *Failed* ]] || break; \
	  sleep 10; \
	done; \
	[[ "$$STATE" == *Complete* || "$$STATE" == *Failed* ]] \
	  || { echo "job/loadtest still running after 65m, past its one-hour deadline. Check it with kubectl -n default describe job loadtest." >&2; exit 1; }; \
	$(KUBECTL) -n default logs -l app.kubernetes.io/name=loadtest --tail=40 --prefix || true; \
	[[ "$$STATE" != *Complete* ]] || exit 0; \
	REASON=$$($(KUBECTL) -n default get job loadtest -o jsonpath='{.status.conditions[?(@.type=="Failed")].reason}') || REASON=; \
	if [[ "$$REASON" == DeadlineExceeded ]]; then \
	  echo "job/loadtest hit its one-hour deadline before k6 finished, so the run was cut short. That is not a failed request: check the pods started in time." >&2; \
	else \
	  echo "job/loadtest failed ($${REASON:-no reason given}): some requests failed. See the summaries above." >&2; \
	fi; \
	exit 1
