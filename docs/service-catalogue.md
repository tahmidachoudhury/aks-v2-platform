# Service Catalogue

**TL;DR:** One entry per service, recorded from `app/services/`. Secrets feed P6-06, events feed P1-01 to P1-05, and the call graph feeds P6-08 NetworkPolicies. Tracked in [#3](https://github.com/tahmidachoudhury/aks-v2-platform/issues/3).

## Summary

| Service                                       | Language | Port | Tables                                    | Publishes                                                 | Consumes | Calls                            |
| --------------------------------------------- | -------- | ---- | ----------------------------------------- | --------------------------------------------------------- | -------- | -------------------------------- |
| [order-service](#order-service)               | Go 1.22  | 8081 | `orders`, `order_events`                  | `order.created`, `order.status_changed`                   | none     | none                             |
| [api-gateway](#api-gateway)                   | Go 1.22  | 8080 | none                                      | none                                                      | none     | Proxies to the six HTTP services |
| [inventory-service](#inventory-service)       | Go 1.22  | 8082 | `products`, `reservations`                | none                                                      | none     | none                             |
| [payment-service](#payment-service)           | Go 1.22  | 8083 | `payments`, `ledger_entries`              | `payment.completed`, `payment.failed`, `payment.refunded` | none     | none                             |
| [notification-service](#notification-service) | Go 1.22  | 8084 | `notifications`, `notification_templates` | none                                                      | none     | none                             |
| [shipping-service](#shipping-service)         | Go 1.22  | 8085 | `shipments`, `tracking_events`            | `shipment.created`, `shipment.<status>`                   | none     | none                             |
| [worker](#worker)                             |          |      |                                           |                                                           |          |                                  |
| [scheduler](#scheduler)                       |          |      |                                           |                                                           |          |                                  |
| [dashboard-api](#dashboard-api)               |          |      |                                           |                                                           |          |                                  |

Seven services connect to one Postgres database, `orders`, as one user, `app`. Only api-gateway and worker have no database.

## Event flow

| Event                  | Published by     | Consumed by                                         |
| ---------------------- | ---------------- | --------------------------------------------------- |
| `order.created`        | order-service    |                                                     |
| `order.status_changed` | order-service    |                                                     |
| `payment.completed`    | payment-service  | worker (log only)                                   |
| `payment.failed`       | payment-service  | worker (log only)                                   |
| `payment.refunded`     | payment-service  | none                                                |
| `shipment.created`     | shipping-service | worker (log only)                                   |
| `shipment.<status>`    | shipping-service | worker handles `shipment.delivered` only (log only) |

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

Product catalogue and stock reservations.

|               |                                                                                                       |
| ------------- | ----------------------------------------------------------------------------------------------------- |
| **Language**  | Go 1.22, `lib/pq`                                                                                     |
| **Port**      | `PORT`, default `8082`                                                                                |
| **Health**    | `/livez` always 200. `/healthz` pings Postgres, 503 if it fails                                       |
| **Called by** | api-gateway (proxies `/api/inventory`), worker (intends `POST /reserve` and `POST /release`, stubbed) |

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
| **Language**  | Go 1.22, `lib/pq`                                                                                                                     |
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

| Variable                                                                       | Required | Default | Secret   | Notes                                                                        |
| ------------------------------------------------------------------------------ | -------- | ------- | -------- | ---------------------------------------------------------------------------- |
| `DATABASE_URL`                                                                 | Yes      | none    | Yes      | Exits on startup if unset. Compose points it at the shared `orders` database |
| `PORT`                                                                         | No       | `8083`  | No       |                                                                              |
| `SQS_QUEUE_URL`                                                                | No       | none    | No       | If unset, events are only logged                                             |
| `AWS_ENDPOINT_URL`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | No       | none    | Keys yes | Set in compose for LocalStack only. The code never reads them                |

**Data:** creates `payments` and `ledger_entries` in `migrate()` at startup and writes both. scheduler and dashboard-api read `payments`.

**Events:** publishes `payment.completed` and `payment.failed` (in `handleCharge`, as `"payment." + status`) and `payment.refunded` (in `handleRefund`) through `publishEvent`. Consumes nothing.

**Outbound calls:** none.

**Notes:**

- `publishEvent` only logs. It has no stub comment like order-service, but it sends nothing either.
- No payment provider. `rand.Intn(10)` decides success, so there is no provider key to manage.
- `/charge` ignores errors from the ledger inserts and the commit, then publishes the event anyway.
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
| **Language**  | Go 1.22, `lib/pq`                                                                                                       |
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
| POST   | `/webhook`                 | Carrier status update. Updates the shipment, adds a tracking event and publishes `shipment.<status>` |
| GET    | `/livez`, `/healthz`       | Health                                                                                               |

**Environment variables**

| Variable                                                                       | Required | Default | Secret   | Notes                                                                        |
| ------------------------------------------------------------------------------ | -------- | ------- | -------- | ---------------------------------------------------------------------------- |
| `DATABASE_URL`                                                                 | Yes      | none    | Yes      | Exits on startup if unset. Compose points it at the shared `orders` database |
| `PORT`                                                                         | No       | `8085`  | No       |                                                                              |
| `SQS_QUEUE_URL`                                                                | No       | none    | No       | If unset, events are only logged                                             |
| `AWS_ENDPOINT_URL`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | No       | none    | Keys yes | Set in compose for LocalStack only. The code never reads them                |

**Data:** creates `shipments` and `tracking_events` in `migrate()` at startup and writes both. dashboard-api reads `shipments`.

**Events:** publishes `shipment.created` (in `createShipment`) and `shipment.<status>` (in `handleCarrierWebhook`) through `publishEvent`. The status comes from the webhook body. Consumes nothing.

**Outbound calls:** none.

**Notes:**

- `publishEvent` only logs, as in payment-service.
- The webhook has no signature check and the gateway lets it through without a JWT. Anyone who can reach it can set any shipment status. That status becomes the event name without validation.
- No carrier API. Tracking numbers are generated locally.
- Schema migrations run on every pod start.

### worker

### scheduler

### dashboard-api
