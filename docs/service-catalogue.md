# Service Catalogue

**TL;DR:** One entry per service, recorded from `app/services/`. Sections feed later cards: secrets for P6-06, events for P1-01 to P1-05, call graph for P6-08 NetworkPolicies. Tracked in [#3](https://github.com/tahmidachoudhury/aks-v2-platform/issues/3).

## Summary

| Service                                       | Language | Port | Tables                   | Publishes                               | Consumes | Calls                            |
| --------------------------------------------- | -------- | ---- | ------------------------ | --------------------------------------- | -------- | -------------------------------- |
| [order-service](#order-service)               | Go 1.22  | 8081 | `orders`, `order_events` | `order.created`, `order.status_changed` | none     | none                             |
| [api-gateway](#api-gateway)                   | Go 1.22  | 8080 | none                     | none                                    | none     | Proxies to the six HTTP services |
| [inventory-service](#inventory-service)       |          |      |                          |                                         |          |                                  |
| [payment-service](#payment-service)           |          |      |                          |                                         |          |                                  |
| [notification-service](#notification-service) |          |      |                          |                                         |          |                                  |
| [shipping-service](#shipping-service)         |          |      |                          |                                         |          |                                  |
| [worker](#worker)                             |          |      |                          |                                         |          |                                  |
| [scheduler](#scheduler)                       |          |      |                          |                                         |          |                                  |
| [dashboard-api](#dashboard-api)               |          |      |                          |                                         |          |                                  |

Seven services connect to one Postgres database, `orders`, as one user, `app`. Only api-gateway and worker have no database.

## Event flow

| Event                  | Published by  | Consumed by |
| ---------------------- | ------------- | ----------- |
| `order.created`        | order-service |             |
| `order.status_changed` | order-service |             |

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

|               |                                                                                 |
| ------------- | ------------------------------------------------------------------------------- |
| **Language**  | Go 1.22, `lib/pq`                                                               |
| **Port**      | `PORT`, default `8081`                                                          |
| **Health**    | `/livez` always 200. `/healthz` pings Postgres, 503 if it fails                 |
| **Called by** | api-gateway (proxies `/api/orders`), worker (intends to `PUT /status`, stubbed) |

**Endpoints**

| Method | Path                 | Purpose                                                                                        |
| ------ | -------------------- | ---------------------------------------------------------------------------------------------- |
| GET    | `/`                  | List orders for the customer in the `X-User-Email` header                                      |
| GET    | `/{id}`              | Get one order                                                                                  |
| POST   | `/`                  | Create an order, publishes `order.created`                                                     |
| PUT    | `/status`            | Move an order to a new status if the state machine allows it, publishes `order.status_changed` |
| GET    | `/livez`, `/healthz` | Health                                                                                         |

**Environment variables**

| Variable                                                                       | Required | Default | Secret   | Notes                                                                                            |
| ------------------------------------------------------------------------------ | -------- | ------- | -------- | ------------------------------------------------------------------------------------------------ |
| `DATABASE_URL`                                                                 | Yes      | none    | Yes      | Exits on startup if unset                                                                        |
| `PORT`                                                                         | No       | `8081`  | No       |                                                                                                  |
| `SQS_QUEUE_URL`                                                                | No       | none    | No       | If unset, events are only logged                                                                 |
| `AWS_ENDPOINT_URL`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | No       | none    | Keys yes | Set in compose for LocalStack only. The code never reads them. Removed with the Service Bus swap |

**Data:** owns `orders` and `order_events`. `migrate()` creates them at startup with `CREATE TABLE IF NOT EXISTS`. Every create and status change writes an `order_events` row.

**Events:** publishes `order.created` (in `createOrder`) and `order.status_changed` (in `handleUpdateStatus`) through `publishEvent`. Consumes nothing.

**Outbound calls:** none.

**Notes:**

- `publishEvent` sends nothing. It builds the JSON and logs it. The SQS `SendMessage` call is a stub ("Students implement actual SQS SendMessage here"). P1-01 writes the real Service Bus publisher, not a client swap.
- Schema migrations run on every pod start. Relevant to P5-04.

### api-gateway

Public entry point. Issues JWTs, rate limits and proxies to the backend services.

|               |                                                                                                    |
| ------------- | -------------------------------------------------------------------------------------------------- |
| **Language**  | Go 1.22, `go-redis/v8`, `golang-jwt/v5`                                                            |
| **Port**      | `PORT`, default `8080`                                                                             |
| **Health**    | `/livez` always 200. `/healthz` always returns 200 `ok`. It checks neither Redis nor the upstreams |
| **Called by** | none. Clients and the dashboard UI call it                                                         |

**Endpoints**

| Method | Path                                                                                                                  | Purpose                                                                       |
| ------ | --------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| POST   | `/auth/login`                                                                                                         | Return a 24-hour JWT for any email and password                               |
| POST   | `/auth/register`                                                                                                      | Return a 24-hour JWT. Needs an email and password. Stores nothing             |
| Any    | `/api/orders/*`, `/api/inventory/*`, `/api/payments/*`, `/api/notifications/*`, `/api/shipping/*`, `/api/dashboard/*` | Rate limit, check the JWT, strip the prefix and proxy to the matching service |
| GET    | `/livez`, `/healthz`                                                                                                  | Health                                                                        |

**Environment variables**

| Variable                                                                                                                                         | Required | Default                   | Secret | Notes                                                                                               |
| ------------------------------------------------------------------------------------------------------------------------------------------------ | -------- | ------------------------- | ------ | --------------------------------------------------------------------------------------------------- |
| `JWT_SECRET`                                                                                                                                     | No       | `change-me-in-production` | Yes    | HS256 signing key. Falls back to the default if unset                                               |
| `REDIS_URL`                                                                                                                                      | No       | none                      | No     | Rate limiting is off if unset, invalid or unreachable at startup. The compose value has no password |
| `PORT`                                                                                                                                           | No       | `8080`                    | No     |                                                                                                     |
| `ORDER_SERVICE_URL`, `INVENTORY_SERVICE_URL`, `PAYMENT_SERVICE_URL`, `NOTIFICATION_SERVICE_URL`, `SHIPPING_SERVICE_URL`, `DASHBOARD_SERVICE_URL` | No       | `http://<service>:<port>` | No     | Proxy targets. The defaults match compose                                                           |

**Data:** no tables. Redis holds `rate:<ip>` counters that expire after a minute.

**Events:** none.

**Outbound calls:** reverse proxy to order-service, inventory-service, payment-service, notification-service, shipping-service and dashboard-api.

**Notes:**

- Login and register accept any credentials. There is no user store.
- These paths skip the JWT check: `/healthz`, `/auth/*`, `/api/shipping/webhook` and any path ending in `/healthz`.
- With a valid JWT, the gateway sets `X-User-Email` from `sub` and `X-User-Role` from `role`. The backends trust `X-User-Email` as given. No service reads `X-User-Role`. Only the gateway should reach the backends (P6-08).
- The rate limit is 100 requests a minute per client IP. The IP comes from `X-Forwarded-For` first, so a client can spoof it.
- The `/api/dashboard` route does not match dashboard-api's paths. See [dashboard-api](#dashboard-api).

### inventory-service

### payment-service

### notification-service

### shipping-service

### worker

### scheduler

### dashboard-api
