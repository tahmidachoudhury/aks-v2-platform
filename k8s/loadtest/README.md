# loadtest

**TL;DR:** A k6 load test for the P6 drills, run with `make loadtest`. Argo CD never applies it. Three k6 pods in `default` send page loads, logins and one order a second each to `https://azure.order-fulfilment.tahmidchoudhury.uk`, through Traefik and api-gateway, as a client would. Any failed request fails the Job, and `make loadtest` exits 1. The drills in [apps](../apps/README.md#proving-it) and [keda](../keda/README.md#proving-it) run it alongside. Filled for P6-03 (#64), P6-04 (#65), P6-09 (#70) and P6-10 (#71).

## Files

| File | Kind | Name | Purpose |
| --- | --- | --- | --- |
| `job.yaml` | `Job` | `loadtest` | Three k6 pods in parallel, run once |
| `script.js` | `ConfigMap` (generated) | `loadtest` | The k6 script, mounted at `/scripts` |
| | `ConfigMap` | `loadtest-settings` | `DURATION`, `RATE` and `TARGET_IP`, written by `make loadtest` each run |

## What it sends

| Scenario | Rate, per pod | Requests | Why |
| --- | --- | --- | --- |
| `browse` | `RATE` a second, 10 by default | `GET /` (the dashboard page) and `POST /auth/login` | The gateway doesn't rate limit these, so they drive its CPU and dashboard-api's for the HPAs |
| `orders` | 1 a second | `POST /api/orders/` with a token | Each order publishes `order.created`, so the worker and KEDA have messages |

The gateway allows 100 requests a minute per client address on `/api/*` ([service catalogue](../../docs/service-catalogue.md#api-gateway)). Each pod is its own address, and one order a second stays under the limit, so a 429 counts as a failure. The limit means three pods can't load the backends hard. That is the limit working, not a gap in the test.

The thresholds are `http_req_failed` at 0 and every check passing. If either fails, k6 exits non-zero and the Job fails (`backoffLimit: 0`). `make loadtest` then prints each pod's summary and exits 1.

## Why in `default`

- The `apps` quota is sized for the services at their HPA maximums ([apps](../apps/README.md#quotas)). Three test pods at 50m each would eat into rollout headroom.
- The pods use the public host and Traefik, as a client does, so they need no NetworkPolicy in `apps`. The host resolves to Traefik's public IP, and Cilium should route it inside the cluster. [Check each pod is its own client](#check-each-pod-is-its-own-client) confirms it.
- They still meet `restricted`: user 12345 (the image's `k6` user, set by number), read-only root, no capabilities, `RuntimeDefault` seccomp, no ServiceAccount token.

The 150m of requests come from the `general` NodePool, so on a full pool a test pod can wait for room. The k6 image is `grafana/k6:2.3.0`, pinned by digest.

The day-to-day certificate is the staging one, which k6 doesn't trust, so the script sets `insecureSkipTLSVerify`. Criterion 8 checks the certificate from outside the cluster.

## Run it

Needs `make up` done and the Ingress serving. From the repo root:

```bash
# 5 minutes, 10 page loads and logins a second per pod.
make loadtest
make loadtest DURATION=10m RATE=40
```

It deletes any earlier `loadtest` Job and its pods, writes `loadtest-settings`, applies this folder and waits for the Job. It then prints the last 40 lines of each pod's log and exits 0 or 1. `DURATION` is whole minutes from `1m` to `55m`: `activeDeadlineSeconds` stops a run at one hour, pod start included, and the target refuses anything longer. `RATE` is a whole number from 1. On a failure it says which kind: requests failed (the Job's `Failed` reason is `BackoffLimitExceeded`), or the run hit the one-hour deadline (`DeadlineExceeded`) and was cut short. It also stops with a message if the Job is deleted mid-run, and gives up after 65 minutes. The finished Job stays for an hour, then Kubernetes deletes it.

### Check each pod is its own client

The per-client rate limit only stays out of the way if the gateway sees three addresses. While a run is going, list the counters in Redis and the k6 pods' addresses:

```bash
K() { kubectl --context aks-v2-platform "$@"; }
K -n data exec redis-0 -- redis-cli --scan --pattern 'rate:*'
K -n default get pods -l app.kubernetes.io/name=loadtest -o wide
```

Expect three `rate:<address>` keys, one for each k6 pod's IP. If you see a single address instead, such as a node's IP or Traefik's public IP, the traffic went out through the Azure load balancer and came back source-NATed. Then all three pods share one counter, the orders hit 429 past 100 a minute, and the run is invalid. Run it again aimed at Traefik's in-cluster address. The host name, and so the TLS name and the Ingress rule, stay the same:

```bash
make loadtest TARGET_IP=$(K -n platform get svc traefik -o jsonpath='{.spec.clusterIP}')
```

and check the keys again.

To stop a run early: `kubectl --context aks-v2-platform -n default delete job loadtest`.

## Validate

```bash
kustomize build k8s/loadtest | kubeconform -strict -summary
node --input-type=module --check < k8s/loadtest/script.js
```

`node --check` only parses the script. k6 runs it.
