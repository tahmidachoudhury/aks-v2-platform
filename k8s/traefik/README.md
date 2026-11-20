# traefik

**TL;DR:** Traefik v3.7, the cluster's only ingress controller, from the `traefik` chart pinned at 42.4.1. Two replicas in `platform`, behind the cluster's Standard Load Balancer on a static public IP that the platform root creates. The Service keeps client addresses (`externalTrafficPolicy: Local`), and the dashboard is reachable only through `kubectl port-forward`. Install it by hand each morning with the command below until ArgoCD takes it over in P7. Filled by P4-03 (#50).

## Install

You need the NodePools from [`../node-pools`](../node-pools/README.md) and the `platform` namespace from [`../namespaces`](../namespaces) (P4-07, #54). From the repo root:

```bash
helm upgrade --install traefik traefik \
  --repo https://traefik.github.io/charts --version 42.4.1 \
  --namespace platform -f k8s/traefik/values.yaml --wait
```

For P7, this maps to an Argo CD Helm source: `repoURL` is the chart repo, `chart` is `traefik`, `targetRevision` is the version, and `values.yaml` is the values file. The chart installs Traefik's CRDs from its `crds/` folder.

## Public IP

The platform root creates a Standard, zone-redundant static IP, `pip-aks-v2-ingress`, in its own group, `rg-aks-v2-ingress`. The Service names both in its `azure-pip-name` and `azure-load-balancer-resource-group` annotations, so the cloud provider uses that IP instead of creating one. For why it lives in platform with its own group, see [Ingress public IP](../../infra/platform/README.md#ingress-public-ip).

```bash
tofu -chdir=infra/platform output -raw ingress_ip_address
```

The IP is new every morning, so don't hard-code it. ExternalDNS points the A records at whatever the Service reports.

## Load balancer and egress

Ingress and egress share the one Standard Load Balancer AKS creates, `kubernetes` in `rg-aks-v2-platform-nodes`, on different public IPs:

- Egress leaves through the outbound rule on the AKS-managed outbound IP. See [Egress](../../docs/architecture-decisions.md#egress).
- Ingress arrives on `pip-aks-v2-ingress` through two load-balancing rules, ports 80 and 443. AKS creates them with outbound SNAT turned off, so new outbound connections still leave through the outbound rule and Traefik's IP never appears as a source address. It isn't on the Service Bus firewall and doesn't need to be.
- The load balancer bills its first five rules at one flat rate. The outbound rule and the two Traefik rules make three, so ingress adds no rule charge. The IP costs about $0.88 a month on the 176-hour schedule.
- The subnet NSG already allows TCP 80 and 443 from the internet (see the [network README](../../infra/modules/network/README.md#nsg-aks-nodes)). AKS adds the per-Service rules to its own NSG on the node NICs.

## Health probes and client addresses

`externalTrafficPolicy: Local` keeps the client's address, so Traefik sees the real source IP rather than a node's. api-gateway's rate limiter relies on that (#128). Traefik doesn't trust an incoming `X-Forwarded-For` by default, so it replaces the client's header with the address it saw.

With `Local`, the cloud provider ignores probe path and protocol annotations. Each load-balancing rule probes HTTP `/healthz` on the Service's `healthCheckNodePort`. That port returns 200 only on nodes with a ready Traefik pod, so other nodes drop out of the pool. `values.yaml` sets the timing explicitly: every 5 seconds, two failures to mark a node down. These are the defaults.

## Dashboard

The dashboard isn't exposed. Its IngressRoute binds only to the `traefik` entrypoint, port 8080, and the Service exposes only 80 and 443. To view it:

```bash
kubectl --context aks-v2-platform -n platform port-forward deploy/traefik 8080:8080
# then open http://localhost:8080/dashboard/
```

Port 8080 is still open to other pods on the pod IPs. The dashboard is read-only, and a NetworkPolicy for `platform` can close it later.

## Sizing and placement

- Two replicas with a PodDisruptionBudget of one unavailable, so node consolidation never takes both.
- A zone spread with `ScheduleAnyway`: the scheduler prefers two zones but doesn't make NAP add a node to get them.
- Requests of 50m CPU and 64Mi each, limit 256Mi. The general NodePool is capped at 4 vCPUs.
- The `platform` ResourceQuota allows 500m of CPU requests and one LoadBalancer Service, which is this one. Everything in `platform` asks for about 230m: Traefik 100m, cert-manager 30m plus 50m for its start-up check Job, ExternalDNS 10m and External Secrets about 40m.
- The system pool is tainted `CriticalAddonsOnly`, so the pods run on NAP nodes.
- The chart's security contexts meet Pod Security `restricted`: non-root, no privilege escalation, all capabilities dropped, read-only root file system, `RuntimeDefault` seccomp.

## Not done here

- HTTP to HTTPS redirect. An entrypoint-level redirect also catches HTTP-01 challenges, which cert-manager answers on port 80. The app Ingress (P6) can add a redirect middleware for its own routes.
- Gateway API. The brief lists it as an extra. The chart supports it with `providers.kubernetesGateway.enabled`.

## Acceptance test

The full test, with ExternalDNS and cert-manager, is in the [cert-manager README](../cert-manager/README.md#acceptance-test). The Traefik part on its own, with `SUB` set to the subscription:

```bash
IP=$(tofu -chdir=infra/platform output -raw ingress_ip_address)

# The Service holds the static IP and keeps client addresses.
kubectl --context aks-v2-platform -n platform get svc traefik \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip} {.spec.externalTrafficPolicy}{"\n"}'

# Traefik answers on the IP. 404 means no route yet, which is expected.
curl -s -o /dev/null -w '%{http_code}\n' "http://$IP/"

# The dashboard port is not reachable from outside. This times out.
curl -s -m 5 "http://$IP:8080/dashboard/" || echo "not exposed"

# The load balancer's probes: HTTP on the health check node port.
az network lb probe list --subscription "$SUB" -g rg-aks-v2-platform-nodes --lb-name kubernetes \
  --query "[].{name:name, protocol:protocol, port:port, path:requestPath, interval:intervalInSeconds}" -o table
```

The first command prints the IP from the output and `Local`. The probe list shows HTTP probes on one port with the path `/healthz`. The outbound rule has no probe.

## Validate

```bash
helm template traefik traefik --repo https://traefik.github.io/charts --version 42.4.1 \
  -n platform -f k8s/traefik/values.yaml | kubeconform -strict -ignore-missing-schemas -summary
```
