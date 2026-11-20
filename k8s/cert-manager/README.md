# cert-manager

**TL;DR:** cert-manager 1.21 from the `cert-manager` chart pinned at v1.21.3, in `platform`, with two ClusterIssuers: `letsencrypt-staging` and `letsencrypt-production`. Both solve HTTP-01 through Traefik, so cert-manager needs no Azure identity. Use staging until a test passes. Production allows 5 certificates for the same set of names every 7 days, and the cluster is rebuilt every weekday, so one hostname uses that up. Filled by P4-04 (#51).

## Install

You need the NodePools, the `platform` namespace from [`../namespaces`](../namespaces) (P4-07, #54) and Traefik. ExternalDNS can come after, but a certificate isn't issued until it has written the A record. The morning order is Traefik, cert-manager, then ExternalDNS. From the repo root, run `make cert-manager`. The [Makefile](../../Makefile) installs the chart, pinned as `CERT_MANAGER_VERSION`, then applies the ClusterIssuers with `kubectl apply -k k8s/cert-manager`.

The chart installs the CRDs (`crds.enabled`), so the ClusterIssuers apply straight after. `crds.keep` stops `helm uninstall` from deleting the CRDs, and every Certificate with them.

For P7, the chart maps to an Argo CD Helm source like the others. `kubectl apply -k k8s/cert-manager` maps to a second, plain Kustomize Application. The `kustomization.yaml` lists only `cluster-issuers.yaml`, so Kustomize ignores `values.yaml`.

## Why HTTP-01

See [TLS certificates](../../docs/architecture-decisions.md#tls-certificates). HTTP-01 gives HTTPS on one real name with nothing in Azure. DNS-01 would add an identity, a federated credential and DNS Zone Contributor, and only pays off for wildcard or non-public names. We need neither.

The ACME account keys live in Secrets in `platform`, cert-manager's cluster resource namespace. The issuers have no email address, since Let's Encrypt stopped sending expiry mail in 2025 and this repo is public.

## How a certificate is issued

1. An Ingress with `cert-manager.io/cluster-issuer: <issuer>` and a `tls` section names a host and a Secret.
2. cert-manager creates a Certificate, then an ACME order. For the HTTP-01 challenge it starts a solver pod and a temporary Ingress with class `traefik` in the same namespace, serving `/.well-known/acme-challenge/<token>` on port 80.
3. ExternalDNS has already pointed the host at Traefik's IP. Let's Encrypt fetches the token over plain HTTP.
4. cert-manager stores the certificate and key in the Secret, and Traefik serves it on 443. Renewal starts 30 days before expiry, which never happens here since the cluster lasts a day.

The solver pod runs in the Ingress's namespace, not in `platform`. Its security context meets Pod Security `restricted`. It asks for 10m CPU and 64Mi, so the `apps` namespace quota must leave room for it.

Port 80 must reach the solver, so Traefik has no entrypoint-level HTTP to HTTPS redirect. See the [Traefik README](../traefik/README.md#not-done-here).

## Rate limits

Let's Encrypt production allows, per [its rate limits page](https://letsencrypt.org/docs/rate-limits/):

| Limit | Value | Here |
| --- | --- | --- |
| New certificates per exact set of names | 5 every 7 days, refilling one every 34 hours | The one that bites. Certificates live in Secrets, which go with the cluster every evening, so each morning issues a new one. Five weekdays use all five |
| New certificates per registered domain | 50 every 7 days | `tahmidchoudhury.uk`. Far off |
| Failed authorisations per name per account | 5 an hour | A broken DNS record or port 80 burns these. Debug on staging |

Renewals through ARI are exempt, but a fresh cluster has nothing to renew. So:

- Point every Ingress at `letsencrypt-staging` day to day. Browsers distrust staging certificates, but the whole flow is exercised.
- Switch the app's host to `letsencrypt-production` only for a demo or a grading run. Keep it under five issues in any seven days, so a reinstall still has room.
- A test hostname, such as `test.` below, is a different set of names from the app's, so testing doesn't use up the app's limit.
- To use production every day, keep the certificate across the night. One way is an External Secrets PushSecret that copies the TLS Secret to Key Vault, and an ExternalSecret that restores it each morning. cert-manager then finds a valid certificate and doesn't order a new one.

## Pod security and sizing

The chart's defaults meet Pod Security `restricted` for the controller, webhook, cainjector and the startupapicheck Job. That means non-root, `RuntimeDefault` seccomp, no privilege escalation, all capabilities dropped and a read-only root file system. The three Deployments ask for 10m CPU each, with 64Mi, 32Mi and 64Mi of memory. The startupapicheck Job sets no resources, so the `platform` LimitRange gives it 50m and 64Mi.

The leader election Lease stays in `kube-system`, the chart default.

## Acceptance test

This tests P4-03, P4-04 and P4-05 together: an Ingress gets an A record from ExternalDNS, then a certificate from Let's Encrypt staging, then from production. It uses the `default` namespace and a test hostname. Set `SUB` to the subscription.

```bash
CTX=aks-v2-platform
HOST=test.azure.order-fulfilment.tahmidchoudhury.uk
IP=$(tofu -chdir=infra/persistent output -raw ingress_ip_address)
NS=$(tofu -chdir=infra/persistent output -json dns_zone_name_servers | jq -er '.[0]')

# Issuers ready: both show READY True.
kubectl --context $CTX get clusterissuers

# A test app behind an Ingress, on staging first.
kubectl --context $CTX create deployment whoami --image=traefik/whoami:v1.12.0
kubectl --context $CTX expose deployment whoami --port=80
kubectl --context $CTX create ingress whoami --class=traefik \
  --rule="$HOST/*=whoami:80,tls=whoami-tls" \
  --annotation cert-manager.io/cluster-issuer=letsencrypt-staging

# P4-05: the A record appears and points at Traefik's IP.
dig +short "$HOST" "@$NS"; echo "expected $IP"

# P4-04, staging: READY turns True within a few minutes.
kubectl --context $CTX get certificate whoami-tls -w
kubectl --context $CTX get secret whoami-tls -o jsonpath='{.data.tls\.crt}' | base64 -d \
  | openssl x509 -noout -issuer -enddate

# Production: switch the issuer, and delete the Secret so a new order starts.
kubectl --context $CTX annotate ingress whoami cert-manager.io/cluster-issuer=letsencrypt-production --overwrite
kubectl --context $CTX delete secret whoami-tls
kubectl --context $CTX get certificate whoami-tls -w

# Served over HTTPS with a trusted certificate. No -k: curl must verify it.
curl -sS -v "https://$HOST/" 2>&1 | grep -E 'issuer:|SSL certificate verify|X-Forwarded-For|X-Real-Ip'
curl -s https://ifconfig.me; echo

# Clean up. ExternalDNS deletes the records within a minute.
kubectl --context $CTX delete ingress,service,deployment whoami
kubectl --context $CTX delete secret whoami-tls
```

Expect this:

- The staging certificate's issuer has `(STAGING)` in its name.
- The production certificate's issuer is a Let's Encrypt intermediate, and curl prints `SSL certificate verify ok`.
- whoami echoes `X-Forwarded-For` and `X-Real-Ip` equal to your public IP, the last line. This confirms `externalTrafficPolicy: Local` (#128).
- If the Certificate stays not ready, `kubectl describe challenge` gives the reason. Usually the A record hasn't propagated, or port 80 doesn't reach Traefik.

This test uses one of the test host's five production certificates for the week.

## Validate

```bash
helm template cert-manager cert-manager --kube-context aks-v2-platform --repo https://charts.jetstack.io --version v1.21.3 \
  -n platform -f k8s/cert-manager/values.yaml | kubeconform -strict -ignore-missing-schemas -summary
kustomize build k8s/cert-manager | kubeconform -strict -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
```
