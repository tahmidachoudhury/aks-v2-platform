package main

import (
	"bytes"
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

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestMain(m *testing.M) {
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

func setWebhookSecret(t *testing.T, secret string) {
	t.Helper()
	orig := webhookSecret
	webhookSecret = secret
	t.Cleanup(func() { webhookSecret = orig })
}

// expectWebhookUpdate sets up the queries a webhook with an accepted status runs.
func expectWebhookUpdate(mock sqlmock.Sqlmock, status string) {
	mock.ExpectQuery(sqlText("SELECT id, order_id FROM shipments WHERE tracking_number = $1")).
		WithArgs("RM123").
		WillReturnRows(sqlmock.NewRows([]string{"id", "order_id"}).AddRow(3, 7))
	mock.ExpectExec(sqlText("UPDATE shipments SET status = $1")).WithArgs(status, 3).
		WillReturnResult(sqlmock.NewResult(0, 1))
	switch status {
	case "delivered":
		mock.ExpectExec(sqlText("SET delivered_at = NOW()")).WithArgs(3).WillReturnResult(sqlmock.NewResult(0, 1))
	case "in_transit":
		mock.ExpectExec(sqlText("SET shipped_at = COALESCE(shipped_at, NOW())")).WithArgs(3).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec(sqlText("INSERT INTO tracking_events")).WithArgs(3, status, "Leeds", "").
		WillReturnResult(sqlmock.NewResult(1, 1))
}

func webhookRequest(secret, status string) *http.Request {
	body := `{"tracking_number":"RM123","status":"` + status + `","location":"Leeds"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	if secret != "" {
		req.Header.Set("X-Webhook-Secret", secret)
	}
	return req
}

// Pins #127: the webhook needs X-Webhook-Secret to match
// SHIPPING_WEBHOOK_SECRET, and rejects everything when that is unset.
func TestCarrierWebhookSecret(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		sent       string
		want       int
	}{
		{"secret unset, none sent", "", "", http.StatusUnauthorized},
		{"secret unset, one sent", "", "anything", http.StatusUnauthorized},
		{"no secret sent", "s3cret", "", http.StatusUnauthorized},
		{"wrong secret", "s3cret", "guess", http.StatusUnauthorized},
		{"prefix of the secret", "s3cret", "s3cre", http.StatusUnauthorized},
		{"secret with extra", "s3cret", "s3cret!", http.StatusUnauthorized},
		{"matching secret", "s3cret", "s3cret", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setWebhookSecret(t, tt.configured)
			mock := mockDB(t)
			if tt.want == http.StatusOK {
				expectWebhookUpdate(mock, "in_transit")
			}

			rec := httptest.NewRecorder()
			handleCarrierWebhook(rec, webhookRequest(tt.sent, "in_transit"))

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// Pins #127: only the four carrier statuses are accepted, since the status
// becomes part of the published event name.
func TestCarrierWebhookStatusAllowlist(t *testing.T) {
	tests := []struct {
		status string
		want   int
	}{
		{"label_created", http.StatusOK},
		{"in_transit", http.StatusOK},
		{"delivered", http.StatusOK},
		{"cancelled", http.StatusOK},
		{"", http.StatusBadRequest},
		{"returned", http.StatusBadRequest},
		{"DELIVERED", http.StatusBadRequest},
		{"delivered.extra", http.StatusBadRequest},
		{"created", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run("status="+tt.status, func(t *testing.T) {
			setWebhookSecret(t, "s3cret")
			mock := mockDB(t)
			if tt.want == http.StatusOK {
				expectWebhookUpdate(mock, tt.status)
			}
			var logs bytes.Buffer
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(io.Discard) })

			rec := httptest.NewRecorder()
			handleCarrierWebhook(rec, webhookRequest("s3cret", tt.status))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			published := strings.Contains(logs.String(), "shipment.")
			if published != (tt.want == http.StatusOK) {
				t.Errorf("published = %v for status %q:\n%s", published, tt.status, logs.String())
			}
		})
	}
}

func TestCarrierWebhookRequest(t *testing.T) {
	tests := []struct {
		name   string
		method string
		body   string
		want   int
	}{
		{name: "wrong method", method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "invalid json", method: http.MethodPost, body: `{`, want: http.StatusBadRequest},
		{name: "unknown tracking number", method: http.MethodPost, body: `{"tracking_number":"NOPE","status":"delivered"}`, want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setWebhookSecret(t, "s3cret")
			mock := mockDB(t)
			if tt.want == http.StatusNotFound {
				mock.ExpectQuery(sqlText("FROM shipments WHERE tracking_number = $1")).WithArgs("NOPE").
					WillReturnError(sql.ErrNoRows)
			}
			req := httptest.NewRequest(tt.method, "/webhook", strings.NewReader(tt.body))
			req.Header.Set("X-Webhook-Secret", "s3cret")
			rec := httptest.NewRecorder()
			handleCarrierWebhook(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

// Pins #126 and #127: every path needs X-User-Email except the health checks
// and the carrier webhook, which carriers call without a JWT.
func TestRequireUser(t *testing.T) {
	tests := []struct {
		path string
		user string
		want int
	}{
		{"/livez", "", http.StatusOK},
		{"/healthz", "", http.StatusOK},
		{"/webhook", "", http.StatusOK},
		{"/shipments", "", http.StatusUnauthorized},
		{"/shipments/3", "", http.StatusUnauthorized},
		{"/track/RM123", "", http.StatusUnauthorized},
		{"/shipments", "alice@example.com", http.StatusOK},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, tt := range tests {
		t.Run(tt.path+" user="+tt.user, func(t *testing.T) {
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

func TestCreateShipment(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantCarrier string
		wantCountry string
		wantPrefix  string
		want        int
	}{
		{name: "invalid json", body: `{`, want: http.StatusBadRequest},
		{name: "no order", body: `{"recipient_name":"Alice"}`, want: http.StatusBadRequest},
		{name: "no recipient", body: `{"order_id":7}`, want: http.StatusBadRequest},
		{name: "defaults to Royal Mail in GB", body: `{"order_id":7,"recipient_name":"Alice","city":"Leeds"}`,
			wantCarrier: "royal_mail", wantCountry: "GB", wantPrefix: "RM", want: http.StatusCreated},
		{name: "named carrier and country", body: `{"order_id":7,"recipient_name":"Alice","city":"Leeds","carrier":"dpd","country":"IE"}`,
			wantCarrier: "dpd", wantCountry: "IE", wantPrefix: "DPD", want: http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if tt.want == http.StatusCreated {
				mock.ExpectQuery(sqlText("INSERT INTO shipments")).
					WithArgs(7, tt.wantCarrier, sqlmock.AnyArg(), "Alice", "", "", "Leeds", "", tt.wantCountry, 0.0, sqlmock.AnyArg()).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
				mock.ExpectExec(sqlText("INSERT INTO tracking_events")).WithArgs(3, "Leeds").
					WillReturnResult(sqlmock.NewResult(1, 1))
			}

			req := httptest.NewRequest(http.MethodPost, "/shipments", strings.NewReader(tt.body))
			req.Header.Set("X-User-Role", "service")
			rec := httptest.NewRecorder()
			handleShipments(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want == http.StatusCreated {
				var got map[string]interface{}
				json.NewDecoder(rec.Body).Decode(&got)
				if tn, _ := got["tracking_number"].(string); !strings.HasPrefix(tn, tt.wantPrefix) {
					t.Errorf("tracking_number = %v, want prefix %s", got["tracking_number"], tt.wantPrefix)
				}
			}
		})
	}
}

func TestGenerateTrackingNumber(t *testing.T) {
	tests := []struct {
		carrier string
		prefix  string
	}{
		{"royal_mail", "RM"},
		{"dpd", "DPD"},
		{"hermes", "EVR"},
		{"ups", "1Z"},
		{"unknown", "RM"},
	}
	for _, tt := range tests {
		t.Run(tt.carrier, func(t *testing.T) {
			got := generateTrackingNumber(tt.carrier)
			if !strings.HasPrefix(got, tt.prefix) || len(got) <= len(tt.prefix) {
				t.Errorf("generateTrackingNumber(%q) = %q, want prefix %q", tt.carrier, got, tt.prefix)
			}
		})
	}
}

func TestHandleTrack(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		found bool
		want  int
	}{
		{name: "no tracking number", path: "/track/", want: http.StatusBadRequest},
		{name: "unknown tracking number", path: "/track/RM123", want: http.StatusNotFound},
		{name: "found", path: "/track/RM123", found: true, want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if tt.path != "/track/" {
				q := mock.ExpectQuery(sqlText("SELECT id, status FROM shipments WHERE tracking_number = $1")).WithArgs("RM123")
				if !tt.found {
					q.WillReturnError(sql.ErrNoRows)
				} else {
					q.WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(3, "in_transit"))
					mock.ExpectQuery(sqlText("FROM tracking_events")).WithArgs(3).WillReturnRows(
						sqlmock.NewRows([]string{"status", "location", "description", "occurred_at"}).
							AddRow("in_transit", "Leeds", "", "t").
							AddRow("label_created", "Leeds", "Shipping label created", "t"))
				}
			}

			rec := httptest.NewRecorder()
			handleTrack(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.found {
				var got struct {
					CurrentStatus string            `json:"current_status"`
					Events        []json.RawMessage `json:"events"`
				}
				json.NewDecoder(rec.Body).Decode(&got)
				if got.CurrentStatus != "in_transit" || len(got.Events) != 2 {
					t.Errorf("response = %+v", got)
				}
			}
		})
	}
}

// NULL columns come back empty, and a scan or row error is a 500 rather than
// a partly filled list
func TestListShipmentsNullsAndErrors(t *testing.T) {
	cols := []string{"id", "order_id", "carrier", "tracking_number", "status", "recipient_name", "city", "country", "estimated_delivery", "created_at"}
	tests := []struct {
		name string
		rows *sqlmock.Rows
		want int
	}{
		{"NULL columns", sqlmock.NewRows(cols).AddRow(3, 7, "royal_mail", nil, "label_created", nil, nil, nil, nil, "t"), http.StatusOK},
		{"scan error", sqlmock.NewRows(cols).AddRow("three", 7, "royal_mail", nil, "label_created", nil, nil, nil, nil, "t"), http.StatusInternalServerError},
		{"row error", sqlmock.NewRows(cols).AddRow(3, 7, "royal_mail", nil, "label_created", nil, nil, nil, nil, "t").RowError(0, errors.New("connection reset")), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			mock.ExpectQuery(sqlText("FROM shipments s ORDER BY")).WillReturnRows(tt.rows)

			req := httptest.NewRequest(http.MethodGet, "/shipments", nil)
			req.Header.Set("X-User-Role", "admin")
			rec := httptest.NewRecorder()
			handleShipments(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want == http.StatusOK {
				var got []map[string]interface{}
				json.NewDecoder(rec.Body).Decode(&got)
				if len(got) != 1 || got[0]["id"] != 3.0 || got[0]["recipient_name"] != "" || got[0]["created_at"] != "t" {
					t.Errorf("shipments = %v", got)
				}
			}
		})
	}
}

func TestCreateShipmentNeedsPrivilege(t *testing.T) {
	mockDB(t) // any SQL fails the test
	req := httptest.NewRequest(http.MethodPost, "/shipments", strings.NewReader(`{"order_id":7,"recipient_name":"Alice"}`))
	req.Header.Set("X-User-Email", "alice@example.com")
	req.Header.Set("X-User-Role", "customer")
	rec := httptest.NewRecorder()
	handleShipments(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// Customers list shipments for their own orders only. Admin and service
// callers list all of them.
func TestListShipments(t *testing.T) {
	cols := []string{"id", "order_id", "carrier", "tracking_number", "status", "recipient_name", "city", "country", "estimated_delivery", "created_at"}
	tests := []struct {
		name     string
		role     string
		wantSQL  string
		wantArgs []driver.Value
	}{
		{"customer", "customer", "FROM shipments s JOIN orders o ON o.id = s.order_id WHERE o.customer_id = $1 ORDER BY", []driver.Value{"alice@example.com"}},
		{"no role is a customer", "", "WHERE o.customer_id = $1 ORDER BY", []driver.Value{"alice@example.com"}},
		{"admin", "admin", "FROM shipments s ORDER BY", nil},
		{"service", "service", "FROM shipments s ORDER BY", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			mock.ExpectQuery(sqlText(tt.wantSQL)).WithArgs(tt.wantArgs...).WillReturnRows(
				sqlmock.NewRows(cols).AddRow(3, 7, "royal_mail", "RM1", "label_created", "Alice", "Leeds", "GB", nil, "t"))

			req := httptest.NewRequest(http.MethodGet, "/shipments", nil)
			req.Header.Set("X-User-Email", "alice@example.com")
			req.Header.Set("X-User-Role", tt.role)
			rec := httptest.NewRecorder()
			handleShipments(rec, req)

			var got []map[string]interface{}
			json.NewDecoder(rec.Body).Decode(&got)
			if rec.Code != http.StatusOK || len(got) != 1 {
				t.Errorf("status = %d, shipments = %v", rec.Code, got)
			}
		})
	}
}

// A shipment for another customer's order looks the same as a missing one.
// Admin and service callers can read any.
func TestHandleShipmentChecksOwnership(t *testing.T) {
	tests := []struct {
		name  string
		role  string
		owner string // owner of order 7; empty: the order row is missing
		want  int
	}{
		{"own order", "customer", "alice@example.com", http.StatusOK},
		{"another customer's order", "customer", "bob@example.com", http.StatusNotFound},
		{"order row missing", "customer", "", http.StatusNotFound},
		{"owner lookup fails", "customer", "error", http.StatusInternalServerError},
		{"admin", "admin", "", http.StatusOK},
		{"service", "service", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			mock.ExpectQuery(sqlText("FROM shipments WHERE id = $1")).WithArgs("3").WillReturnRows(
				sqlmock.NewRows([]string{"id", "order_id", "carrier", "tracking_number", "status", "recipient_name", "city", "country", "weight_kg", "estimated_delivery", "created_at"}).
					AddRow(3, 7, "royal_mail", "RM1", "label_created", "Alice", "Leeds", "GB", 1.5, "2026-11-12", "t"))
			if tt.role == "customer" {
				q := mock.ExpectQuery(sqlText("SELECT customer_id FROM orders WHERE id = $1")).WithArgs(7)
				switch tt.owner {
				case "":
					q.WillReturnError(sql.ErrNoRows)
				case "error":
					q.WillReturnError(errors.New("db down"))
				default:
					q.WillReturnRows(sqlmock.NewRows([]string{"customer_id"}).AddRow(tt.owner))
				}
			}

			req := httptest.NewRequest(http.MethodGet, "/shipments/3", nil)
			req.Header.Set("X-User-Email", "alice@example.com")
			req.Header.Set("X-User-Role", tt.role)
			rec := httptest.NewRecorder()
			handleShipment(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// The nullable columns can all be NULL without a 404
func TestHandleShipmentNullColumns(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("FROM shipments WHERE id = $1")).WithArgs("3").WillReturnRows(
		sqlmock.NewRows([]string{"id", "order_id", "carrier", "tracking_number", "status", "recipient_name", "city", "country", "weight_kg", "estimated_delivery", "created_at"}).
			AddRow(3, 7, "royal_mail", nil, "label_created", nil, nil, nil, nil, nil, "2026-11-09T10:00:00Z"))

	req := httptest.NewRequest(http.MethodGet, "/shipments/3", nil)
	req.Header.Set("X-User-Role", "admin")
	rec := httptest.NewRecorder()
	handleShipment(rec, req)

	var got map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&got)
	if rec.Code != http.StatusOK || got["id"] != 3.0 || got["tracking_number"] != "" || got["weight_kg"] != 0.0 {
		t.Errorf("status = %d, shipment = %v", rec.Code, got)
	}
}

func TestHandleShipmentQueryError(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("FROM shipments WHERE id = $1")).WithArgs("3").WillReturnError(errors.New("db down"))

	req := httptest.NewRequest(http.MethodGet, "/shipments/3", nil)
	req.Header.Set("X-User-Role", "admin")
	rec := httptest.NewRecorder()
	handleShipment(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestHandleShipmentNotFound(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("FROM shipments WHERE id = $1")).WithArgs("99").WillReturnError(sql.ErrNoRows)

	rec := httptest.NewRecorder()
	handleShipment(rec, httptest.NewRequest(http.MethodGet, "/shipments/99", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleHealth(t *testing.T) {
	tests := []struct {
		name         string
		shuttingDown bool
		pingErr      error
		sbDown       bool
		want         int
	}{
		{"healthy", false, nil, false, http.StatusOK},
		{"database down", false, errors.New("db down"), false, http.StatusServiceUnavailable},
		{"service bus down", false, nil, true, http.StatusServiceUnavailable},
		// #19: readiness fails on SIGTERM so the pod leaves the endpoints
		{"shutting down", true, nil, false, http.StatusServiceUnavailable},
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
		})
	}
}

// unreachableServiceBus points sbClient at a port nothing listens on.
func unreachableServiceBus(t *testing.T) {
	t.Helper()
	client, err := azservicebus.NewClientFromConnectionString("Endpoint=sb://127.0.0.1:1;SharedAccessKeyName=test;SharedAccessKey=test;UseDevelopmentEmulator=true;", nil)
	if err != nil {
		t.Fatal(err)
	}
	sbClient, sbQueue = client, "order-events"
	t.Cleanup(func() {
		client.Close(context.Background())
		sbClient, sbQueue = nil, ""
	})
}
