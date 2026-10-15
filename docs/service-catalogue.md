# Service Catalogue

**TL;DR:** One entry per service, recorded from `app/services/`. Per-service sections feed later cards: secrets for P6-06, events for P1-01 to P1-05, call graph for P6-08 NetworkPolicies. Tracked in [#3](https://github.com/tahmidachoudhury/aks-v2-platform/issues/3).

## Summary

| Service | Language | Port | Tables | Publishes | Consumes | Calls |
|---|---|---|---|---|---|---|
| [order-service](#order-service) | Go 1.22 | 8081 | `orders`, `order_events` | `order.created`, `order.status_changed` | none | none |
| [api-gateway](#api-gateway) | | | | | | |
| [inventory-service](#inventory-service) | | | | | | |
| [payment-service](#payment-service) | | | | | | |
| [notification-service](#notification-service) | | | | | | |
| [shipping-service](#shipping-service) | | | | | | |
| [worker](#worker) | | | | | | |
| [scheduler](#scheduler) | | | | | | |
| [dashboard-api](#dashboard-api) | | | | | | |

## Event flow

| Event | Published by | Consumed by |
|---|---|---|
| `order.created` | order-service | |
| `order.status_changed` | order-service | |

## Services

<!--
Template for each entry. Copy it and keep the headings. Write "none" for an empty field.

### service-name

Purpose in one line.

| | |
|---|---|
| **Language** | |
| **Port** | `PORT`, default |
| **Health** | liveness and readiness paths, and what readiness checks |
| **Called by** | |

**Endpoints**

| Method | Path | Purpose |
|---|---|---|

**Environment variables**

| Variable | Required | Default | Secret | Notes |
|---|---|---|---|---|

**Data:** tables owned, read or written.

**Events:** published and consumed, with where in the code.

**Outbound calls:** other services this one calls over HTTP.

**Notes:** anything that affects the platform build.
-->

### order-service

Order lifecycle and the status state machine.

| | |
|---|---|
| **Language** | Go 1.22, `lib/pq` |
| **Port** | `PORT`, default `8081` |
| **Health** | `/livez` always 200. `/healthz` pings Postgres, 503 if it fails |
| **Called by** | api-gateway (proxies `/api/orders`), worker (intends to `PUT /status`, stubbed) |

**Endpoints**

| Method | Path | Purpose |
|---|---|---|
| GET | `/` | List orders for the customer in the `X-User-Email` header |
| GET | `/{id}` | Get one order |
| POST | `/` | Create an order, publishes `order.created` |
| PUT | `/status` | Move an order to a new status if the state machine allows it, publishes `order.status_changed` |
| GET | `/livez`, `/healthz` | Health |

**Environment variables**

| Variable | Required | Default | Secret | Notes |
|---|---|---|---|---|
| `DATABASE_URL` | Yes | none | Yes | Exits on startup if unset |
| `PORT` | No | `8081` | No | |
| `SQS_QUEUE_URL` | No | none | No | If unset, events are only logged |
| `AWS_ENDPOINT_URL`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | No | none | Keys yes | Set in compose for LocalStack only. The code never reads them. Removed with the Service Bus swap |

**Data:** owns `orders` and `order_events`. `migrate()` creates them at startup with `CREATE TABLE IF NOT EXISTS`. Every create and status change writes an `order_events` row.

**Events:** publishes `order.created` (in `createOrder`) and `order.status_changed` (in `handleUpdateStatus`) through `publishEvent`. Consumes nothing.

**Outbound calls:** none.

**Notes:**
- `publishEvent` sends nothing. It builds the JSON and logs it. The SQS `SendMessage` call is a stub ("Students implement actual SQS SendMessage here"). P1-01 writes the real Service Bus publisher, not a client swap.
- Schema migrations run on every pod start. Relevant to P5-04.

### api-gateway

### inventory-service

### payment-service

### notification-service

### shipping-service

### worker

### scheduler

### dashboard-api
