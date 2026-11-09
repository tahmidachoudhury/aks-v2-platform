# Service Catalogue

**TL;DR:** One entry per service, recorded from `app/services/`. Secrets feed P6-06, events feed P1-01 to P1-05, and the call graph feeds P6-08 NetworkPolicies. Tracked in [#3](https://github.com/tahmidachoudhury/aks-v2-platform/issues/3).

## Summary

| Service                                       | Language | Port               | Tables                                                             | Publishes                                                 | Consumes                | Calls                            |
| --------------------------------------------- | -------- | ------------------ | ------------------------------------------------------------------ | --------------------------------------------------------- | ----------------------- | -------------------------------- |
| [order-service](#order-service)               | Go 1.22  | 8081               | `orders`, `order_events`                                           | `order.created`, `order.status_changed`                   | none                    | none                             |
| [api-gateway](#api-gateway)                   | Go 1.22  | 8080               | none                                                               | none                                                      | none                    | Proxies to the six HTTP services |
| [inventory-service](#inventory-service)       | Go 1.22  | 8082               | `products`, `reservations`                                         | none                                                      | none                    | none                             |
| [payment-service](#payment-service)           | Go 1.22  | 8083               | `payments`, `ledger_entries`                                       | `payment.completed`, `payment.failed`, `payment.refunded` | none                    | none                             |
| [notification-service](#notification-service) | Go 1.22  | 8084               | `notifications`, `notification_templates`                          | none                                                      | none                    | none                             |
| [shipping-service](#shipping-service)         | Go 1.22  | 8085               | `shipments`, `tracking_events`                                     | `shipment.created`, `shipment.<status>`                   | none                    | none                             |
| [worker](#worker)                             | Go 1.22  | 8090 (health only) | none                                                               | none                                                      | 6 event types, log only | none (5 services stubbed)        |
| [scheduler](#scheduler)                       | Go 1.22  | 8091 (health only) | none (writes `order_events`, `products`, `reservations`)           | none                                                      | none                    | order-service, inventory-service |
| [dashboard-api](#dashboard-api)               | Go 1.22  | 8086               | none (reads `orders`, `payments`, `products`, `shipments`)         | none                                                      | none                    | none                             |

Seven services connect to one Postgres database, `orders`, as one user, `app`. Only api-gateway and worker have no database.

## Event flow

| Event                  | Published by     | Consumed by                                         |
| ---------------------- | ---------------- | --------------------------------------------------- |
| `order.created`        | order-service    | worker (log only)                                   |
| `order.status_changed` | order-service    | worker (log only)                                   |
| `payment.completed`    | payment-service  | worker (log only)                                   |
| `payment.failed`       | payment-service  | worker (log only)                                   |
| `payment.refunded`     | payment-service  | none                                                |
| `shipment.created`     | shipping-service | worker (log only)                                   |
| `shipment.<status>`    | shipping-service | worker handles `shipment.delivered` only (log only) |

Every event goes to one Service Bus queue, `order-events`. The worker receives all of them, but its handlers only log, so no event changes anything downstream yet.

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
| **Language**  | Go 1.22, `lib/pq`, `azservicebus`, `azidentity`                                 |
| **Port**      | `PORT`, default `8081`                                                          |
| **Health**    | `/livez` always 200. `/healthz` pings Postgres, 503 if it fails                 |
| **Called by** | api-gateway (proxies `/api/orders`), scheduler (`PUT /status`), worker (intends to `PUT /status`, stubbed) |

**Endpoints**

| Method | Path                 | Purpose                                                                                        |
| ------ | -------------------- | ---------------------------------------------------------------------------------------------- |
| GET    | `/`                  | List orders for the customer in the `X-User-Email` header                                      |
| GET    | `/{id}`              | Get one order. 404 if it belongs to another customer                                           |
| POST   | `/`                  | Create an order, publishes `order.created`                                                     |
| PUT    | `/status`            | Move an order to a new status if the state machine allows it, publishes `order.status_changed` |
| GET    | `/livez`, `/healthz` | Health                                                                                         |

**Environment variables**

| Variable                       | Required | Default | Secret | Notes                                                                                                                                    |
| ------------------------------ | -------- | ------- | ------ | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `DATABASE_URL`                 | Yes      | none    | Yes    | Exits on startup if unset                                                                                                                |
| `PORT`                         | No       | `8081`  | No     |                                                                                                                                          |
| `SERVICEBUS_QUEUE`             | No       | none    | No     | Queue name. Compose sets `order-events`. If unset, or if neither connection setting is set, events are only logged                       |
| `SERVICEBUS_CONNECTION_STRING` | No       | none    | Yes    | Local emulator only. Used in place of `SERVICEBUS_NAMESPACE` when set. Compose uses the emulator's fixed local string                    |
| `SERVICEBUS_NAMESPACE`         | No       | none    | No     | Fully qualified namespace, such as `sb-x.servicebus.windows.net`. Authenticates with `DefaultAzureCredential` (Workload Identity in AKS) |

**Data:** creates `orders` and `order_events` in `migrate()` at startup with `CREATE TABLE IF NOT EXISTS`. It is the only service that inserts into them. Every create and status change writes an `order_events` row. It is not the only writer: scheduler DELETEs `order_events` rows older than 90 days. scheduler and dashboard-api read `orders`.

**Events:** publishes `order.created` (in `createOrder`) and `order.status_changed` (in `handleUpdateStatus`) through `publishEvent`. Consumes nothing.

**Outbound calls:** none.

**Notes:**

- `publishEvent` sends the `{type, payload, timestamp}` JSON to Service Bus, with `Subject` set to the event type. The sender is created once at startup. A failed send is logged and the request still succeeds, so an event is lost if the bus is down. There is no outbox.
- Schema migrations run on every pod start. Relevant to P5-04.
- A schema change to `orders` also touches scheduler and dashboard-api.
- Every path except `/livez` and `/healthz` returns 401 without `X-User-Email`. `PUT /status` does not check the customer. The header can be forged by anything that reaches the service directly, so only the gateway should reach it (P6-08).

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
| GET    | `/`                                                                                                                   | The dashboard UI page, proxied from dashboard-api. No JWT check or rate limit |
| GET    | `/livez`, `/healthz`                                                                                                  | Health                                                                        |

**Environment variables**

| Variable                                                                                                                                         | Required | Default                   | Secret | Notes                                                                                               |
| ------------------------------------------------------------------------------------------------------------------------------------------------ | -------- | ------------------------- | ------ | --------------------------------------------------------------------------------------------------- |
| `JWT_SECRET`                                                                                                                                     | Yes      | none                      | Yes    | HS256 signing key. Exits on startup if unset                                                        |
| `REDIS_URL`                                                                                                                                      | No       | none                      | No     | Rate limiting is off if unset, invalid or unreachable at startup. The compose value has no password |
| `PORT`                                                                                                                                           | No       | `8080`                    | No     |                                                                                                     |
| `TRUSTED_PROXIES`                                                                                                                                | No       | none                      | No     | Comma-separated CIDRs of proxies, such as Traefik, whose `X-Forwarded-For` the rate limit trusts. Exits on startup if an entry is invalid. Not set in compose |
| `ORDER_SERVICE_URL`, `INVENTORY_SERVICE_URL`, `PAYMENT_SERVICE_URL`, `NOTIFICATION_SERVICE_URL`, `SHIPPING_SERVICE_URL`, `DASHBOARD_SERVICE_URL` | No       | `http://<service>:<port>` | No     | Proxy targets. The defaults match compose                                                           |

**Data:** no tables. Redis holds `rate:<ip>` counters that expire after a minute.

**Events:** none.

**Outbound calls:** reverse proxy to order-service, inventory-service, payment-service, notification-service, shipping-service and dashboard-api. dashboard-api also serves the UI page at `/`.

**Notes:**

- Login and register accept any credentials. There is no user store.
- These paths skip the JWT check: `/healthz`, `/auth/*`, `/api/shipping/webhook` and any path ending in `/healthz`.
- The gateway drops any `X-User-Email` or `X-User-Role` the client sends. With a valid JWT, it sets `X-User-Email` from `sub` and `X-User-Role` from `role`. order-service, inventory-service, payment-service, notification-service and shipping-service return 401 without `X-User-Email`, except on `/livez`, `/healthz` and shipping's `/webhook`. They trust the value as given. No service reads `X-User-Role`. Only the gateway should reach the backends (P6-08).
- The rate limit is 100 requests a minute per client IP. The IP is the TCP peer address without the port. When the peer is in `TRUSTED_PROXIES`, the IP is the right-most `X-Forwarded-For` entry that is not itself a trusted proxy, so a client cannot choose its own counter. With `TRUSTED_PROXIES` unset, the header is ignored.
- `/api/dashboard/*` reaches dashboard-api as `/dashboard/*`. The gateway strips `/api/dashboard` and the proxy target ends in `/dashboard`.

### inventory-service

Product catalogue and stock reservations.

|               |                                                                                                       |
| ------------- | ----------------------------------------------------------------------------------------------------- |
| **Language**  | Go 1.22, `lib/pq`                                                                                     |
| **Port**      | `PORT`, default `8082`                                                                                |
| **Health**    | `/livez` always 200. `/healthz` pings Postgres, 503 if it fails                                       |
| **Called by** | api-gateway (proxies `/api/inventory`), scheduler (`POST /release`), worker (intends `POST /reserve` and `POST /release`, stubbed) |

**Endpoints**

| Method | Path                 | Purpose                                                                                            |
| ------ | -------------------- | -------------------------------------------------------------------------------------------------- |
| GET    | `/products`          | List up to 100 products by name, with available stock                                              |
| POST   | `/products`          | Create a product                                                                                   |
| Any    | `/products/{id}`     | Get one product                                                                                    |
| POST   | `/reserve`           | Reserve stock for an order in one transaction with row locks. Reservations expire after 15 minutes |
| POST   | `/release`           | Release an order's active reservations                                                             |
| Any    | `/low-stock`         | Products with under 10 available                                                                   |
| GET    | `/livez`, `/healthz` | Health                                                                                             |

**Environment variables**

| Variable       | Required | Default | Secret | Notes                                                                        |
| -------------- | -------- | ------- | ------ | ---------------------------------------------------------------------------- |
| `DATABASE_URL` | Yes      | none    | Yes    | Exits on startup if unset. Compose points it at the shared `orders` database |
| `PORT`         | No       | `8082`  | No     |                                                                              |

**Data:** creates `products` and `reservations` in `migrate()` at startup and writes both. scheduler also UPDATEs both to expire reservations. dashboard-api reads `products`.

**Events:** none.

**Outbound calls:** none.

**Notes:**

- Reservation expiry relies on scheduler's `expire_reservations` job. This service never expires reservations itself.
- No role check. Any signed-in user can create products through the gateway.
- Schema migrations run on every pod start.

### payment-service

Simulated charges and refunds, with a double-entry ledger.

|               |                                                                                                                                       |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| **Language**  | Go 1.22, `lib/pq`, `azservicebus`, `azidentity`                                                                                       |
| **Port**      | `PORT`, default `8083`                                                                                                                |
| **Health**    | `/livez` always 200. `/healthz` pings Postgres, 503 if it fails                                                                       |
| **Called by** | api-gateway (proxies `/api/payments`), worker (intends `POST /charge` and `POST /refund`, stubbed), scheduler (comment only, no code) |

**Endpoints**

| Method | Path                     | Purpose                                                                                                                                                                       |
| ------ | ------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| POST   | `/charge`                | Simulate a charge that succeeds 9 times in 10. Writes the payment and, on success, two ledger rows. Publishes `payment.completed` or `payment.failed`. Returns 402 on failure |
| POST   | `/refund`                | Refund a completed payment in full or in part. Publishes `payment.refunded`                                                                                                   |
| Any    | `/ledger`                | Latest 100 ledger entries                                                                                                                                                     |
| Any    | `/balance/{customer_id}` | Charged, refunded and net totals for one customer                                                                                                                             |
| GET    | `/livez`, `/healthz`     | Health                                                                                                                                                                        |

**Environment variables**

| Variable                       | Required | Default | Secret | Notes                                                                                                                                    |
| ------------------------------ | -------- | ------- | ------ | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `DATABASE_URL`                 | Yes      | none    | Yes    | Exits on startup if unset. Compose points it at the shared `orders` database                                                             |
| `PORT`                         | No       | `8083`  | No     |                                                                                                                                          |
| `SERVICEBUS_QUEUE`             | No       | none    | No     | Queue name. Compose sets `order-events`. If unset, or if neither connection setting is set, events are only logged                       |
| `SERVICEBUS_CONNECTION_STRING` | No       | none    | Yes    | Local emulator only. Used in place of `SERVICEBUS_NAMESPACE` when set. Compose uses the emulator's fixed local string                    |
| `SERVICEBUS_NAMESPACE`         | No       | none    | No     | Fully qualified namespace, such as `sb-x.servicebus.windows.net`. Authenticates with `DefaultAzureCredential` (Workload Identity in AKS) |

**Data:** creates `payments` and `ledger_entries` in `migrate()` at startup and writes both. scheduler and dashboard-api read `payments`.

**Events:** publishes `payment.completed` and `payment.failed` (in `handleCharge`, as `"payment." + status`) and `payment.refunded` (in `handleRefund`) through `publishEvent`. Consumes nothing.

**Outbound calls:** none.

**Notes:**

- `publishEvent` works as in order-service.
- No payment provider. `rand.Intn(10)` decides success, so there is no provider key to manage.
- `/charge` returns 500 and publishes nothing if a ledger insert or the commit fails.
- Schema migrations run on every pod start.

### notification-service

Simulated email and SMS sends from stored templates.

|               |                                                                                                                       |
| ------------- | --------------------------------------------------------------------------------------------------------------------- |
| **Language**  | Go 1.22, `lib/pq`                                                                                                     |
| **Port**      | `PORT`, default `8084`                                                                                                |
| **Health**    | `/livez` always 200. `/healthz` pings Postgres, 503 if it fails                                                       |
| **Called by** | api-gateway (proxies `/api/notifications`), worker (intends `POST /send`, stubbed), scheduler (comment only, no code) |

**Endpoints**

| Method | Path                 | Purpose                                                             |
| ------ | -------------------- | ------------------------------------------------------------------- |
| POST   | `/send`              | Look up the template, record the notification as `sent` and log it  |
| Any    | `/history`           | Latest 50 notifications, filtered by the `recipient` query if given |
| Any    | `/templates`         | List templates                                                      |
| GET    | `/livez`, `/healthz` | Health                                                              |

**Environment variables**

| Variable       | Required | Default | Secret | Notes                                                                        |
| -------------- | -------- | ------- | ------ | ---------------------------------------------------------------------------- |
| `DATABASE_URL` | Yes      | none    | Yes    | Exits on startup if unset. Compose points it at the shared `orders` database |
| `PORT`         | No       | `8084`  | No     |                                                                              |

**Data:** creates `notifications` and `notification_templates` in `migrate()` at startup and writes both. It seeds seven templates with `ON CONFLICT DO NOTHING`. No other service touches either table.

**Events:** none.

**Outbound calls:** none.

**Notes:**

- Nothing is sent. There is no SES, SNS or other provider, so no provider credentials yet.
- Templates are not rendered. The stored body keeps its `{{...}}` placeholders and the request data goes into `metadata`.
- Schema migrations and template seeding run on every pod start.

### shipping-service

Shipments, tracking and carrier status webhooks.

|               |                                                                                                                         |
| ------------- | ----------------------------------------------------------------------------------------------------------------------- |
| **Language**  | Go 1.22, `lib/pq`, `azservicebus`, `azidentity`                                                                         |
| **Port**      | `PORT`, default `8085`                                                                                                  |
| **Health**    | `/livez` always 200. `/healthz` pings Postgres, 503 if it fails                                                         |
| **Called by** | api-gateway (proxies `/api/shipping`, with `/api/shipping/webhook` public), worker (intends `POST /shipments`, stubbed) |

**Endpoints**

| Method | Path                       | Purpose                                                                                              |
| ------ | -------------------------- | ---------------------------------------------------------------------------------------------------- |
| GET    | `/shipments`               | Latest 100 shipments                                                                                 |
| POST   | `/shipments`               | Create a shipment with a generated tracking number. Publishes `shipment.created`                     |
| Any    | `/shipments/{id}`          | Get one shipment                                                                                     |
| Any    | `/track/{tracking_number}` | Current status and tracking events                                                                   |
| POST   | `/webhook`                 | Carrier status update. Checks `X-Webhook-Secret`, updates the shipment, adds a tracking event and publishes `shipment.<status>` |
| GET    | `/livez`, `/healthz`       | Health                                                                                               |

**Environment variables**

| Variable                       | Required | Default | Secret | Notes                                                                                                                                    |
| ------------------------------ | -------- | ------- | ------ | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `DATABASE_URL`                 | Yes      | none    | Yes    | Exits on startup if unset. Compose points it at the shared `orders` database                                                             |
| `PORT`                         | No       | `8085`  | No     |                                                                                                                                          |
| `SHIPPING_WEBHOOK_SECRET`      | No       | none    | Yes    | Shared secret carriers send in `X-Webhook-Secret`. If unset, `/webhook` rejects every request. Compose sets a local value
| `SERVICEBUS_QUEUE`             | No       | none    | No     | Queue name. Compose sets `order-events`. If unset, or if neither connection setting is set, events are only logged                       |
| `SERVICEBUS_CONNECTION_STRING` | No       | none    | Yes    | Local emulator only. Used in place of `SERVICEBUS_NAMESPACE` when set. Compose uses the emulator's fixed local string                    |
| `SERVICEBUS_NAMESPACE`         | No       | none    | No     | Fully qualified namespace, such as `sb-x.servicebus.windows.net`. Authenticates with `DefaultAzureCredential` (Workload Identity in AKS) |

**Data:** creates `shipments` and `tracking_events` in `migrate()` at startup and writes both. dashboard-api reads `shipments`.

**Events:** publishes `shipment.created` (in `createShipment`) and `shipment.<status>` (in `handleCarrierWebhook`) through `publishEvent`. The status comes from the webhook body and must be `label_created`, `in_transit`, `delivered` or `cancelled`. Consumes nothing.

**Outbound calls:** none.

**Notes:**

- `publishEvent` works as in order-service.
- The gateway lets the webhook through without a JWT. The service returns 401 unless `X-Webhook-Secret` matches `SHIPPING_WEBHOOK_SECRET`, compared in constant time, and 400 for a status outside the four above. It is a shared secret, not a signature over the body.
- No carrier API. Tracking numbers are generated locally.
- Schema migrations run on every pod start.

### worker

Event consumer meant to drive the order flow across services. It receives from Service Bus, but its handlers only log.

|               |                                                                                      |
| ------------- | ------------------------------------------------------------------------------------ |
| **Language**  | Go 1.22, `azservicebus`, `azidentity`                                                |
| **Port**      | No `PORT`. Health server on `HEALTH_PORT`, default `8090`. Compose publishes no port |
| **Health**    | `/livez` always 200. `/healthz` always returns 200 `ok`. It does not check the queue |
| **Called by** | none                                                                                 |

**Endpoints**

| Method | Path                 | Purpose |
| ------ | -------------------- | ------- |
| GET    | `/livez`, `/healthz` | Health  |

**Environment variables**

| Variable                                                                                                                | Required         | Default                   | Secret | Notes                                                                                                                                    |
| ----------------------------------------------------------------------------------------------------------------------- | ---------------- | ------------------------- | ------ | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `SERVICEBUS_QUEUE`                                                                                                      | Yes              | none                      | No     | Exits on startup if unset. Compose sets `order-events`                                                                                   |
| `SERVICEBUS_CONNECTION_STRING`                                                                                          | One of these two | none                      | Yes    | Local emulator only. Used in place of `SERVICEBUS_NAMESPACE` when set. Exits on startup if both are unset                                |
| `SERVICEBUS_NAMESPACE`                                                                                                  | One of these two | none                      | No     | Fully qualified namespace, such as `sb-x.servicebus.windows.net`. Authenticates with `DefaultAzureCredential` (Workload Identity in AKS) |
| `HEALTH_PORT`                                                                                                           | No               | `8090`                    | No     | Not set in compose                                                                                                                       |
| `ORDER_SERVICE_URL`, `INVENTORY_SERVICE_URL`, `PAYMENT_SERVICE_URL`, `NOTIFICATION_SERVICE_URL`, `SHIPPING_SERVICE_URL` | No               | `http://<service>:<port>` | No     | Read into a map that no code uses yet                                                                                                    |

**Data:** none.

**Events:** consumes through the `handleEvent` switch: `order.created`, `order.status_changed` (branches on `new_status`: `processing`, `shipped`, `delivered`, `cancelled`), `payment.completed`, `payment.failed`, `shipment.created` and `shipment.delivered`. Every case only logs. Other types, such as `payment.refunded`, hit the default and are skipped. Publishes nothing.

**Outbound calls:** none implemented. Comments describe calls to inventory-service (`/reserve`, `/release`), payment-service (`/charge`, `/refund`), notification-service (`/send`), shipping-service (`/shipments`) and order-service (`PUT /status`). The HTTP client is created but unused.

**Notes:**

- Receives up to 10 messages at a time in PeekLock mode. A message is completed when `handleEvent` succeeds and abandoned on any failure, including a body that does not parse.
- Compose runs the Service Bus emulator. Its queue dead-letters a message after 3 deliveries and on expiry (`servicebus-emulator/Config.json`).
- No database.

### scheduler

Timed background jobs that work directly on other services' tables.

|               |                                                                                      |
| ------------- | ------------------------------------------------------------------------------------ |
| **Language**  | Go 1.22, `lib/pq`                                                                    |
| **Port**      | No `PORT`. Health server on `HEALTH_PORT`, default `8091`. Compose publishes no port |
| **Health**    | `/livez` always 200. `/healthz` pings Postgres, 503 if it fails                      |
| **Called by** | none                                                                                 |

**Endpoints**

| Method | Path                 | Purpose |
| ------ | -------------------- | ------- |
| GET    | `/livez`, `/healthz` | Health  |

**Environment variables**

| Variable       | Required | Default | Secret | Notes                                                                        |
| -------------- | -------- | ------- | ------ | ---------------------------------------------------------------------------- |
| `DATABASE_URL` | Yes      | none    | Yes    | Exits on startup if unset. Compose points it at the shared `orders` database |
| `HEALTH_PORT`  | No       | `8091`  | No     | Not set in compose                                                           |
| `ORDER_SERVICE_URL`, `INVENTORY_SERVICE_URL` | No | `http://<service>:<port>` | No | For cancelling abandoned orders. The defaults match compose |

**Data:** creates nothing and runs no migrations. UPDATEs `products` and `reservations` (expiry). DELETEs from `order_events`. Reads `orders`, `reservations` and `payments`.

**Events:** none directly. Cancelling through order-service publishes `order.status_changed`.

**Outbound calls:** order-service `PUT /status` and inventory-service `POST /release` for abandoned orders, sent with `X-User-Email: scheduler`. Comments mention payment-service `/charge` for retries and notification-service for the digest, with no code.

**Notes:**

- Each job runs once at startup and then on a ticker:
  - `expire_reservations`, every minute: releases expired active reservations in `products` and `reservations`.
  - `abandoned_carts`, every 5 minutes: cancels `pending` orders older than 30 minutes through order-service's `PUT /status`, then releases their stock through inventory-service's `POST /release`. If the cancel fails, the order stays `pending` and the next run retries it. If the release fails, `expire_reservations` frees the stock when the reservation expires.
  - `retry_failed_payments`, every 15 minutes: counts failed payments from the last hour and logs the count. It retries nothing.
  - `daily_digest`, every hour: logs order and revenue counts.
  - `cleanup_old_events`, every 30 minutes: deletes `order_events` rows older than 90 days.
- No locking or leader election. Every replica runs every job, so run one replica.
- It needs the tables that order-service, inventory-service and payment-service create. Until they exist, most jobs log query errors.
- A schema change in those three services needs a scheduler check.

### dashboard-api

Admin dashboard UI and read-only analytics over the shared database.

|               |                                                                                                          |
| ------------- | -------------------------------------------------------------------------------------------------------- |
| **Language**  | Go 1.22, `lib/pq`. The UI is embedded from `static/`                                                     |
| **Port**      | `PORT`, default `8086`                                                                                   |
| **Health**    | `/livez` always 200. `/healthz` and `/dashboard/healthz` ping Postgres, 503 if it fails                  |
| **Called by** | api-gateway (proxies `/api/dashboard/*` to `/dashboard/*`, and `/` for the UI page) |

**Endpoints**

| Method | Path                                       | Purpose                                                     |
| ------ | ------------------------------------------ | ----------------------------------------------------------- |
| Any    | `/dashboard/summary`                       | Headline counts for orders, revenue, stock and shipments    |
| Any    | `/dashboard/orders/stats`                  | Orders and order value per day for the last 30 days         |
| Any    | `/dashboard/revenue`                       | Charges, refunds, net and daily revenue for the last 7 days |
| Any    | `/dashboard/inventory/alerts`              | Products with under 10 available                            |
| Any    | `/dashboard/shipping/overview`             | Shipments by status and carrier, and average delivery hours |
| GET    | `/`                                        | Static UI (`static/index.html`)                             |
| GET    | `/livez`, `/healthz`, `/dashboard/healthz` | Health                                                      |

**Environment variables**

| Variable       | Required | Default | Secret | Notes                                                                        |
| -------------- | -------- | ------- | ------ | ---------------------------------------------------------------------------- |
| `DATABASE_URL` | Yes      | none    | Yes    | Exits on startup if unset. Compose points it at the shared `orders` database |
| `PORT`         | No       | `8086`  | No     |                                                                              |

**Data:** creates and writes nothing. Reads `orders`, `payments`, `products` and `shipments`.

**Events:** none.

**Outbound calls:** none from the server. The UI calls its own origin (`window.location.origin`) for `/auth/login` and `/api/*`, including `/api/dashboard/*`, with the JWT from its auto-login.

**Notes:**

- The UI loads from api-gateway at `/`, so one origin serves the page, `/auth/*` and `/api/*`. The ingress or HTTPRoute sends every path to api-gateway and none to dashboard-api. Under compose, open the UI at `http://localhost:8080/`.
- `/dashboard/*`, except `/dashboard/healthz`, returns 401 without `X-User-Email`, as on the other backends. Through the gateway that means a valid JWT. Anything that reaches dashboard-api directly can forge the header, so only the gateway should reach it (P6-08).
- Every signed-in user can read the dashboard data. Login only issues the `customer` role and nothing checks `X-User-Role`.
- It reads four tables that four other services own. A schema change there needs a dashboard check.
