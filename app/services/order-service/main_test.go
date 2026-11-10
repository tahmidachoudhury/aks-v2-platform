package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestMain(m *testing.M) {
	// publishEvent and the handlers log on every call
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// mockDB points the package-level db at a sqlmock and checks every
// expectation was met when the test ends. Pings are expectations too.
func mockDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	conn, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	db = conn
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet SQL expectations: %v", err)
		}
		conn.Close()
	})
	return mock
}

func sqlText(s string) string { return regexp.QuoteMeta(s) }

var orderColumns = []string{"id", "customer_id", "status", "items", "total", "currency", "notes", "created_at", "updated_at"}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// Pins #126: every path but the health checks needs X-User-Email.
func TestRequireUser(t *testing.T) {
	tests := []struct {
		name string
		path string
		user string
		want int
	}{
		{"livez is exempt", "/livez", "", http.StatusOK},
		{"healthz is exempt", "/healthz", "", http.StatusOK},
		{"list without user", "/", "", http.StatusUnauthorized},
		{"get without user", "/42", "", http.StatusUnauthorized},
		{"status without user", "/status", "", http.StatusUnauthorized},
		{"list with user", "/", "alice@example.com", http.StatusOK},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.user != "" {
				req.Header.Set("X-User-Email", tt.user)
			}
			rec := httptest.NewRecorder()
			requireUser(next).ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

// Pins #126: GET / returns only the caller's orders, unless the caller is an
// admin or an internal service.
func TestListOrdersFiltersByCaller(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		role     string
		wantSQL  string
		wantArgs []driver.Value
	}{
		{
			name:     "all of the caller's orders",
			url:      "/",
			wantSQL:  "FROM orders WHERE 1=1 AND customer_id = $1 ORDER BY created_at DESC",
			wantArgs: []driver.Value{"alice@example.com"},
		},
		{
			name:     "status filter keeps the caller filter",
			url:      "/?status=shipped",
			wantSQL:  "FROM orders WHERE 1=1 AND customer_id = $1 AND status = $2 ORDER BY",
			wantArgs: []driver.Value{"alice@example.com", "shipped"},
		},
		{
			name:     "customer role is filtered",
			url:      "/",
			role:     "customer",
			wantSQL:  "FROM orders WHERE 1=1 AND customer_id = $1 ORDER BY",
			wantArgs: []driver.Value{"alice@example.com"},
		},
		{
			name:    "admin sees every order",
			url:     "/",
			role:    "admin",
			wantSQL: "FROM orders WHERE 1=1 ORDER BY created_at DESC",
		},
		{
			name:     "service with a status filter",
			url:      "/?status=pending",
			role:     "service",
			wantSQL:  "FROM orders WHERE 1=1 AND status = $1 ORDER BY",
			wantArgs: []driver.Value{"pending"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			mock.ExpectQuery("^" + sqlText("SELECT id, customer_id, status, items, total, currency, notes, created_at, updated_at "+tt.wantSQL)).WithArgs(tt.wantArgs...).WillReturnRows(
				sqlmock.NewRows(orderColumns).
					AddRow(1, "alice@example.com", "shipped", []byte(`[{"product_id":"p1","quantity":1,"price":5}]`), 5.0, "GBP", "", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"),
			)

			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			req.Header.Set("X-User-Email", "alice@example.com")
			req.Header.Set("X-User-Role", tt.role)
			rec := httptest.NewRecorder()
			handleOrders(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
			}
			var orders []Order
			decode(t, rec, &orders)
			if len(orders) != 1 || orders[0].CustomerID != "alice@example.com" {
				t.Errorf("orders = %+v", orders)
			}
		})
	}
}

func TestListOrdersQueryError(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("FROM orders")).WillReturnError(errors.New("db down"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-Email", "alice@example.com")
	rec := httptest.NewRecorder()
	handleOrders(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// Pins #126: GET /{id} checks the caller owns the order, and another
// customer's order looks the same as a missing one. Admin and service callers
// can read any order.
func TestGetOrderChecksOwnership(t *testing.T) {
	tests := []struct {
		name  string
		owner string // empty: the order does not exist
		role  string
		want  int
	}{
		{"caller's own order", "alice@example.com", "customer", http.StatusOK},
		{"another customer's order", "bob@example.com", "customer", http.StatusNotFound},
		{"no role is a customer", "bob@example.com", "", http.StatusNotFound},
		{"missing order", "", "customer", http.StatusNotFound},
		{"admin reads any order", "bob@example.com", "admin", http.StatusOK},
		{"service reads any order", "bob@example.com", "service", http.StatusOK},
		{"unknown role is not privileged", "bob@example.com", "superuser", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			q := mock.ExpectQuery(sqlText("FROM orders WHERE id = $1")).WithArgs("7")
			if tt.owner != "" {
				q.WillReturnRows(sqlmock.NewRows(orderColumns).
					AddRow(7, tt.owner, "pending", []byte(`[]`), 10.0, "GBP", "", "t", "t"))
			} else {
				q.WillReturnError(sql.ErrNoRows)
			}

			req := httptest.NewRequest(http.MethodGet, "/7", nil)
			req.Header.Set("X-User-Email", "alice@example.com")
			req.Header.Set("X-User-Role", tt.role)
			rec := httptest.NewRecorder()
			handleOrders(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestCreateOrder(t *testing.T) {
	const items = `[{"product_id":"p1","quantity":2,"price":10},{"product_id":"p2","quantity":1,"price":5.5}]`
	tests := []struct {
		name      string
		user      string
		body      string
		dbErr     error
		want      int
		wantTotal float64
	}{
		{name: "no customer", body: `{"items":` + items + `}`, want: http.StatusBadRequest},
		{name: "invalid json", user: "alice@example.com", body: `{`, want: http.StatusBadRequest},
		{name: "no items", user: "alice@example.com", body: `{"items":[]}`, want: http.StatusBadRequest},
		{name: "insert fails", user: "alice@example.com", body: `{"items":` + items + `}`, dbErr: errors.New("db down"), want: http.StatusInternalServerError},
		{name: "created", user: "alice@example.com", body: `{"items":` + items + `}`, want: http.StatusCreated, wantTotal: 25.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if tt.want == http.StatusCreated || tt.dbErr != nil {
				// The total is price x quantity summed, and the currency defaults to GBP
				q := mock.ExpectQuery(sqlText("INSERT INTO orders")).
					WithArgs("alice@example.com", sqlmock.AnyArg(), 25.5, "GBP", "")
				if tt.dbErr != nil {
					q.WillReturnError(tt.dbErr)
				} else {
					q.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(42))
					mock.ExpectExec(sqlText("INSERT INTO order_events")).WithArgs(42).
						WillReturnResult(sqlmock.NewResult(1, 1))
				}
			}

			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			if tt.user != "" {
				req.Header.Set("X-User-Email", tt.user)
			}
			rec := httptest.NewRecorder()
			handleOrders(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want == http.StatusCreated {
				var got struct {
					ID     int     `json:"id"`
					Status string  `json:"status"`
					Total  float64 `json:"total"`
				}
				decode(t, rec, &got)
				if got.ID != 42 || got.Status != "pending" || got.Total != tt.wantTotal {
					t.Errorf("response = %+v", got)
				}
			}
		})
	}
}

func TestHandleOrdersMethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	handleOrders(rec, httptest.NewRequest(http.MethodDelete, "/1", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHandleUpdateStatus(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		body      string
		current   string // empty: the order does not exist
		owner     string // default alice@example.com, the caller
		role      string // default service
		updateErr error
		want      int
	}{
		{name: "wrong method", method: http.MethodPost, body: `{}`, want: http.StatusMethodNotAllowed},
		{name: "invalid json", method: http.MethodPut, body: `{`, want: http.StatusBadRequest},
		{name: "unknown order", method: http.MethodPut, body: `{"order_id":9,"new_status":"confirmed"}`, want: http.StatusNotFound},
		{name: "pending to confirmed", method: http.MethodPut, body: `{"order_id":9,"new_status":"confirmed"}`, current: "pending", want: http.StatusOK},
		{name: "pending to cancelled", method: http.MethodPut, body: `{"order_id":9,"new_status":"cancelled"}`, current: "pending", want: http.StatusOK},
		{name: "processing to shipped", method: http.MethodPut, body: `{"order_id":9,"new_status":"shipped"}`, current: "processing", want: http.StatusOK},
		{name: "shipped to delivered", method: http.MethodPut, body: `{"order_id":9,"new_status":"delivered"}`, current: "shipped", want: http.StatusOK},
		{name: "pending cannot skip to shipped", method: http.MethodPut, body: `{"order_id":9,"new_status":"shipped"}`, current: "pending", want: http.StatusConflict},
		{name: "shipped cannot be cancelled", method: http.MethodPut, body: `{"order_id":9,"new_status":"cancelled"}`, current: "shipped", want: http.StatusConflict},
		{name: "delivered is final", method: http.MethodPut, body: `{"order_id":9,"new_status":"cancelled"}`, current: "delivered", want: http.StatusConflict},
		{name: "cancelled is final", method: http.MethodPut, body: `{"order_id":9,"new_status":"confirmed"}`, current: "cancelled", want: http.StatusConflict},
		{name: "unknown current status", method: http.MethodPut, body: `{"order_id":9,"new_status":"confirmed"}`, current: "lost", want: http.StatusBadRequest},
		{name: "update fails", method: http.MethodPut, body: `{"order_id":9,"new_status":"confirmed"}`, current: "pending", updateErr: errors.New("db down"), want: http.StatusInternalServerError},
		// Customers change only their own orders. Admin and service callers,
		// such as the scheduler, change any.
		{name: "another customer's order", method: http.MethodPut, body: `{"order_id":9,"new_status":"cancelled"}`, current: "pending", owner: "bob@example.com", role: "customer", want: http.StatusNotFound},
		// Customers may only cancel their own order, so they cannot walk it
		// through to delivered without paying
		{name: "customer cancels their own order", method: http.MethodPut, body: `{"order_id":9,"new_status":"cancelled"}`, current: "pending", role: "customer", want: http.StatusOK},
		{name: "customer cannot confirm their own order", method: http.MethodPut, body: `{"order_id":9,"new_status":"confirmed"}`, role: "customer", want: http.StatusForbidden},
		{name: "customer cannot mark their order delivered", method: http.MethodPut, body: `{"order_id":9,"new_status":"delivered"}`, role: "customer", want: http.StatusForbidden},
		{name: "no role cannot confirm", method: http.MethodPut, body: `{"order_id":9,"new_status":"confirmed"}`, role: "none", want: http.StatusForbidden},
		{name: "admin on another customer's order", method: http.MethodPut, body: `{"order_id":9,"new_status":"cancelled"}`, current: "pending", owner: "bob@example.com", role: "admin", want: http.StatusOK},
		{name: "service on another customer's order", method: http.MethodPut, body: `{"order_id":9,"new_status":"cancelled"}`, current: "pending", owner: "bob@example.com", role: "service", want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			role := tt.role
			switch role {
			case "":
				role = "service"
			case "none":
				role = ""
			}
			if tt.method == http.MethodPut && tt.body != `{` && tt.want != http.StatusForbidden {
				q := mock.ExpectQuery(sqlText("SELECT status, customer_id FROM orders WHERE id = $1")).WithArgs(9)
				owner := tt.owner
				if owner == "" {
					owner = "alice@example.com"
				}
				if tt.current == "" {
					q.WillReturnError(sql.ErrNoRows)
				} else {
					q.WillReturnRows(sqlmock.NewRows([]string{"status", "customer_id"}).AddRow(tt.current, owner))
				}
			}
			if tt.want == http.StatusOK || tt.updateErr != nil {
				var req struct {
					NewStatus string `json:"new_status"`
				}
				json.Unmarshal([]byte(tt.body), &req)
				u := mock.ExpectExec(sqlText("UPDATE orders SET status = $1")).WithArgs(req.NewStatus, 9)
				if tt.updateErr != nil {
					u.WillReturnError(tt.updateErr)
				} else {
					u.WillReturnResult(sqlmock.NewResult(0, 1))
					mock.ExpectExec(sqlText("INSERT INTO order_events")).
						WithArgs(9, tt.current, req.NewStatus).
						WillReturnResult(sqlmock.NewResult(1, 1))
				}
			}

			req := httptest.NewRequest(tt.method, "/status", strings.NewReader(tt.body))
			req.Header.Set("X-User-Email", "alice@example.com")
			req.Header.Set("X-User-Role", role)
			rec := httptest.NewRecorder()
			handleUpdateStatus(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestHandleHealth(t *testing.T) {
	tests := []struct {
		name           string
		shuttingDown   bool
		pingErr        error
		sbDown         bool
		want           int
		wantStatus     string
		wantServiceBus string
	}{
		{"healthy", false, nil, false, http.StatusOK, "ok", "disabled"},
		{"database down", false, errors.New("db down"), false, http.StatusServiceUnavailable, "unhealthy", "disabled"},
		// Service Bus is report-only: the service still serves without it.
		// handleHealth reads the stored result, so this opens no link.
		{"service bus down", false, nil, true, http.StatusOK, "ok", "degraded"},
		// #19: readiness fails on SIGTERM so the pod leaves the endpoints
		{"shutting down", true, nil, false, http.StatusServiceUnavailable, "shutting down", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if !tt.shuttingDown {
				mock.ExpectPing().WillReturnError(tt.pingErr)
			}
			if tt.sbDown {
				unreachableServiceBus(t)
			}
			shuttingDown.Store(tt.shuttingDown)
			t.Cleanup(func() { shuttingDown.Store(false) })

			rec := httptest.NewRecorder()
			handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			var body map[string]string
			json.NewDecoder(rec.Body).Decode(&body)
			if body["status"] != tt.wantStatus || body["servicebus"] != tt.wantServiceBus {
				t.Errorf("body = %v, want status %q servicebus %q", body, tt.wantStatus, tt.wantServiceBus)
			}
		})
	}
}

// watchServiceBus stores the result of a check on each tick, and stops when
// the ticks stop.
func TestWatchServiceBus(t *testing.T) {
	unreachableServiceBus(t)
	sbUp.Store(true)
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		watchServiceBus(ticks)
		close(done)
	}()
	ticks <- time.Now()
	close(ticks)
	<-done
	if sbUp.Load() {
		t.Error("sbUp = true after a failed check, want false")
	}
}

// unreachableServiceBus points sbClient at a port nothing listens on, with
// the stored check result down.
func unreachableServiceBus(t *testing.T) {
	t.Helper()
	client, err := azservicebus.NewClientFromConnectionString("Endpoint=sb://127.0.0.1:1;SharedAccessKeyName=test;SharedAccessKey=test;UseDevelopmentEmulator=true;", nil)
	if err != nil {
		t.Fatal(err)
	}
	sbClient, sbQueue = client, "order-events"
	sbUp.Store(false)
	t.Cleanup(func() {
		client.Close(context.Background())
		sbClient, sbQueue = nil, ""
		sbUp.Store(false)
	})
}

func TestPublishEventWithoutServiceBus(t *testing.T) {
	// With no sender configured, publishing only logs and must not panic
	sbSender = nil
	publishEvent("order.created", map[string]interface{}{"order_id": 1})
}
