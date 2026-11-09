# Bug fix screenshots

**TL;DR:** Before and after evidence for the nine bug fixes on `tc/service-bug-fixes`, for [#20](https://github.com/tahmidachoudhury/aks-v2-platform/issues/20). "Before" images come from `main` (574859d), "after" images from the branch (961657d), both under docker compose. All output was captured from the running stack. JWTs and the webhook secret are truncated.

Most images are terminal panels showing the request and the real output. The two #129 UI images show the dashboard in Chromium, with the page's own requests listed underneath.

## #122: dashboard routes return 404 through the gateway

[#122](https://github.com/tahmidachoudhury/aks-v2-platform/issues/122). `GET /api/dashboard/summary` with a JWT returns 404 before the fix and the summary JSON after it.

![#122 before](122-before.png)
![#122 after](122-after.png)

## #129: dashboard data has no auth

[#129](https://github.com/tahmidachoudhury/aks-v2-platform/issues/129). Before, `:8086/dashboard/summary` returns revenue and order data with no token. After, it returns 401 directly and 401 through the gateway without a JWT, and 200 with one.

![#129 before](129-before.png)
![#129 after](129-after.png)

The dashboard UI under compose. On `main`, `:8080/` returns 401, and `:8086/` shows the stats with no login while every `/auth` and `/api` call 404s. After the fix, `:8080/` serves the page, it logs in, and every call and health dot is green.

![#129 UI before](129-ui-before.png)
![#129 UI after](129-ui-after.png)

## #125: JWT_SECRET falls back to a public default

[#125](https://github.com/tahmidachoudhury/aks-v2-platform/issues/125). Before, the gateway starts with an empty `JWT_SECRET`, and a token forged with `change-me-in-production` reads alice's orders. After, the gateway exits with `JWT_SECRET is required`, and the forged token gets 401.

![#125 before](125-before.png)
![#125 after](125-after.png)

## #128: rate limit key comes from X-Forwarded-For

[#128](https://github.com/tahmidachoudhury/aks-v2-platform/issues/128). 150 requests, each with a new forged `X-Forwarded-For`. Before: 150 × 200 and 150 Redis keys. After: 100 × 200, 50 × 429 and one key.

![#128 before](128-before.png)
![#128 after](128-after.png)

## #126: backends trust X-User-Email

[#126](https://github.com/tahmidachoudhury/aks-v2-platform/issues/126). Before, order-service returns every order to a request with no `X-User-Email`, and bob reads alice's order. After, the request with no header gets 401, bob gets 404 on alice's order, and bob still reads his own.

![#126 before](126-before.png)
![#126 after](126-after.png)

## #124: charge ignores ledger and commit errors

[#124](https://github.com/tahmidachoudhury/aks-v2-platform/issues/124). A `CHECK (false)` constraint makes every ledger insert fail. Before, the charge returns 201 `completed` with no payment row, and the worker confirms the order. After, it returns 500 and publishes no event.

![#124 before](124-before.png)
![#124 after](124-after.png)

## #127: shipping webhook has no auth

[#127](https://github.com/tahmidachoudhury/aks-v2-platform/issues/127). Before, a webhook with no secret and status `delivered.FAKE` returns 200 and writes that status to the shipment. After, it gets 401 without the secret and 400 for the invented status, and a known status still works.

![#127 before](127-before.png)
![#127 after](127-after.png)

## #123: scheduler cancels orders outside the state machine

[#123](https://github.com/tahmidachoudhury/aks-v2-platform/issues/123). The scheduler cancels a pending order with 3 widgets reserved, backdated 31 minutes. Before, there is no `order_events` row and the reservation stays active. After, there is a `pending → cancelled` row and the reservation is released.

![#123 before](123-before.png)
![#123 after](123-after.png)

## #19: graceful shutdown on SIGTERM

[#19](https://github.com/tahmidachoudhury/aks-v2-platform/issues/19). SIGTERM to order-service under load through the gateway. Before, the process exits at once and the gateway returns 502s. After, requests keep returning 200 through the 5s drain, while `/healthz` returns 503 and `/livez` returns 200. Compose has no readiness check, so the 502s after the drain, once the container has exited, are expected here.

![#19 before](19-before.png)
![#19 after](19-after.png)
