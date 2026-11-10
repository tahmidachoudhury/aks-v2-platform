package main

import (
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

func TestHandleSend(t *testing.T) {
	tests := []struct {
		name                      string
		method                    string
		body                      string
		template                  bool   // the template exists in the database
		storedSubject, storedBody string // default wantSubject, wantBody
		wantChannel               string
		wantSubject               string
		wantBody                  string
		want                      int
	}{
		{name: "wrong method", method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "invalid json", method: http.MethodPost, body: `{`, want: http.StatusBadRequest},
		{name: "no recipient", method: http.MethodPost, body: `{"template":"order_shipped"}`, want: http.StatusBadRequest},
		{name: "no template", method: http.MethodPost, body: `{"recipient":"alice@example.com"}`, want: http.StatusBadRequest},
		{
			name: "stored template, default channel", method: http.MethodPost,
			body:     `{"recipient":"alice@example.com","template":"order_shipped","data":{"order_id":7}}`,
			template: true, wantChannel: "email", wantSubject: "Shipped", wantBody: "Your order has shipped", want: http.StatusCreated,
		},
		{
			name: "placeholders filled from data", method: http.MethodPost,
			body:     `{"recipient":"alice@example.com","template":"order_shipped","data":{"OrderID":7,"TrackingNumber":"RM123"}}`,
			template: true, storedSubject: "Shipped - #{{.OrderID}}", storedBody: "Order #{{.OrderID}} shipped. Tracking: {{.TrackingNumber}}",
			wantChannel: "email", wantSubject: "Shipped - #7", wantBody: "Order #7 shipped. Tracking: RM123", want: http.StatusCreated,
		},
		{
			name: "numbers render as written", method: http.MethodPost,
			body:     `{"recipient":"alice@example.com","template":"order_confirmed","data":{"OrderID":1234567,"Total":12.50}}`,
			template: true, storedSubject: "#{{.OrderID}}", storedBody: "Total {{.Total}}",
			wantChannel: "email", wantSubject: "#1234567", wantBody: "Total 12.50", want: http.StatusCreated,
		},
		{
			name: "missing placeholder data", method: http.MethodPost,
			body:     `{"recipient":"alice@example.com","template":"order_shipped","data":{"OrderID":7}}`,
			template: true, storedSubject: "Shipped - #{{.OrderID}}", storedBody: "Tracking: {{.TrackingNumber}}", want: http.StatusBadRequest,
		},
		{
			name: "no data for a template with placeholders", method: http.MethodPost,
			body:     `{"recipient":"alice@example.com","template":"order_shipped"}`,
			template: true, storedSubject: "Shipped - #{{.OrderID}}", storedBody: "x", want: http.StatusBadRequest,
		},
		{
			name: "unknown template name is not rendered", method: http.MethodPost,
			body:        `{"recipient":"alice@example.com","template":"{{.OrderID}}"}`,
			wantChannel: "email", wantSubject: "{{.OrderID}}", wantBody: "{{.OrderID}}", want: http.StatusCreated,
		},
		{
			name: "unknown template falls back to its name", method: http.MethodPost,
			body:        `{"recipient":"+447700900000","channel":"sms","template":"custom_text"}`,
			wantChannel: "sms", wantSubject: "custom_text", wantBody: "custom_text", want: http.StatusCreated,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			var in struct {
				Recipient string `json:"recipient"`
				Template  string `json:"template"`
			}
			json.Unmarshal([]byte(tt.body), &in)
			if tt.template || tt.want == http.StatusCreated {
				q := mock.ExpectQuery(sqlText("FROM notification_templates WHERE id = $1")).WithArgs(in.Template)
				if tt.template {
					subject, body := tt.storedSubject, tt.storedBody
					if subject == "" {
						subject, body = tt.wantSubject, tt.wantBody
					}
					q.WillReturnRows(sqlmock.NewRows([]string{"subject", "body"}).AddRow(subject, body))
				} else {
					q.WillReturnError(sql.ErrNoRows)
				}
			}
			if tt.want == http.StatusCreated {
				mock.ExpectQuery(sqlText("INSERT INTO notifications")).
					WithArgs(in.Recipient, tt.wantChannel, in.Template, tt.wantSubject, tt.wantBody, sqlmock.AnyArg(), "sent", sqlmock.AnyArg()).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(5))
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, "/send", strings.NewReader(tt.body))
			req.Header.Set("X-User-Role", "service")
			handleSend(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want == http.StatusCreated {
				var got map[string]interface{}
				json.NewDecoder(rec.Body).Decode(&got)
				if got["id"] != 5.0 || got["channel"] != tt.wantChannel || got["status"] != "sent" {
					t.Errorf("response = %v", got)
				}
			}
		})
	}
}

func TestHandleHistory(t *testing.T) {
	cols := []string{"id", "recipient", "channel", "template", "subject", "status", "sent_at", "created_at"}
	// The caller is alice
	tests := []struct {
		name     string
		url      string
		role     string
		wantSQL  string
		wantArgs []driver.Value
	}{
		{"admin sees everyone's", "/history", "admin", "FROM notifications ORDER BY created_at DESC", nil},
		{"admin picks a recipient", "/history?recipient=bob@example.com", "admin", "FROM notifications WHERE recipient = $1 ORDER BY", []driver.Value{"bob@example.com"}},
		{"service sees everyone's", "/history", "service", "FROM notifications ORDER BY created_at DESC", nil},
		{"customer sees their own", "/history", "customer", "FROM notifications WHERE recipient = $1 ORDER BY", []driver.Value{"alice@example.com"}},
		{"customer names themselves", "/history?recipient=alice@example.com", "customer", "FROM notifications WHERE recipient = $1 ORDER BY", []driver.Value{"alice@example.com"}},
		{"no role is a customer", "/history", "", "FROM notifications WHERE recipient = $1 ORDER BY", []driver.Value{"alice@example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			q := mock.ExpectQuery(sqlText(tt.wantSQL))
			if tt.wantArgs != nil {
				q.WithArgs(tt.wantArgs...)
			} else {
				q.WithoutArgs()
			}
			q.WillReturnRows(sqlmock.NewRows(cols).
				AddRow(1, "alice@example.com", "email", "order_shipped", "Shipped", "sent", "t", "t").
				AddRow(2, "alice@example.com", "email", "order_shipped", "Shipped", "queued", nil, "t"))

			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			req.Header.Set("X-User-Email", "alice@example.com")
			req.Header.Set("X-User-Role", tt.role)
			rec := httptest.NewRecorder()
			handleHistory(rec, req)

			var got []struct {
				SentAt string `json:"sent_at"`
			}
			json.NewDecoder(rec.Body).Decode(&got)
			if rec.Code != http.StatusOK || len(got) != 2 {
				t.Fatalf("status = %d, notifications = %+v", rec.Code, got)
			}
			if got[1].SentAt != "" {
				t.Errorf("NULL sent_at = %q, want empty", got[1].SentAt)
			}
		})
	}
}

func TestHandleHistoryOtherRecipient(t *testing.T) {
	mockDB(t) // any SQL fails the test
	req := httptest.NewRequest(http.MethodGet, "/history?recipient=bob@example.com", nil)
	req.Header.Set("X-User-Email", "alice@example.com")
	req.Header.Set("X-User-Role", "customer")
	rec := httptest.NewRecorder()
	handleHistory(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestHandleSendTemplateLookupFails(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("FROM notification_templates WHERE id = $1")).WillReturnError(errors.New("db down"))

	req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(`{"recipient":"alice@example.com","template":"order_shipped"}`))
	req.Header.Set("X-User-Role", "service")
	rec := httptest.NewRecorder()
	handleSend(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
}

// The seeded templates must all parse, and render with their fields filled
func TestRenderSeededPlaceholders(t *testing.T) {
	data := map[string]interface{}{"OrderID": 7.0, "CustomerName": "Alice", "Currency": "GBP", "Total": 12.5,
		"TrackingNumber": "RM1", "ProductName": "Mug", "SKU": "MUG-1", "Available": 3.0}
	got, err := render("Hi {{.CustomerName}}, order #{{.OrderID}} total {{.Currency}} {{.Total}}", data)
	if err != nil || got != "Hi Alice, order #7 total GBP 12.5" {
		t.Errorf("render = %q, %v", got, err)
	}
	if _, err := render("{{.OrderID", data); err == nil {
		t.Error("a template that does not parse must be an error")
	}
}

// A failed INSERT returns 500, not 201 with id 0
func TestHandleSendInsertFails(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("FROM notification_templates WHERE id = $1")).WithArgs("custom").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(sqlText("INSERT INTO notifications")).WillReturnError(errors.New("db down"))

	req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(`{"recipient":"alice@example.com","template":"custom"}`))
	req.Header.Set("X-User-Role", "service")
	rec := httptest.NewRecorder()
	handleSend(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
}

func TestHandleSendNeedsPrivilege(t *testing.T) {
	mockDB(t) // any SQL fails the test
	req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(`{"recipient":"bob@example.com","template":"x"}`))
	req.Header.Set("X-User-Email", "alice@example.com")
	req.Header.Set("X-User-Role", "customer")
	rec := httptest.NewRecorder()
	handleSend(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestHandleHistoryQueryError(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("FROM notifications")).WillReturnError(errors.New("db down"))

	rec := httptest.NewRecorder()
	handleHistory(rec, httptest.NewRequest(http.MethodGet, "/history", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestHandleTemplates(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		want  int
		count int
	}{
		{"lists templates", nil, http.StatusOK, 2},
		{"query fails", errors.New("db down"), http.StatusInternalServerError, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			q := mock.ExpectQuery(sqlText("FROM notification_templates ORDER BY id"))
			if tt.err != nil {
				q.WillReturnError(tt.err)
			} else {
				q.WillReturnRows(sqlmock.NewRows([]string{"id", "channel", "subject", "body"}).
					AddRow("order_confirmed", "email", "Confirmed", "Hi").
					AddRow("order_shipped_sms", "sms", "", "Shipped"))
			}

			rec := httptest.NewRecorder()
			handleTemplates(rec, httptest.NewRequest(http.MethodGet, "/templates", nil))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.err == nil {
				var got []map[string]string
				json.NewDecoder(rec.Body).Decode(&got)
				if len(got) != tt.count {
					t.Errorf("templates = %v", got)
				}
			}
		})
	}
}

// Pins #126: every path but the health checks needs X-User-Email.
func TestRequireUser(t *testing.T) {
	tests := []struct {
		path string
		user string
		want int
	}{
		{"/livez", "", http.StatusOK},
		{"/healthz", "", http.StatusOK},
		{"/send", "", http.StatusUnauthorized},
		{"/history", "", http.StatusUnauthorized},
		{"/templates", "", http.StatusUnauthorized},
		{"/history", "alice@example.com", http.StatusOK},
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

func TestHandleHealth(t *testing.T) {
	tests := []struct {
		name         string
		shuttingDown bool
		pingErr      error
		want         int
	}{
		{"healthy", false, nil, http.StatusOK},
		{"database down", false, errors.New("db down"), http.StatusServiceUnavailable},
		// #19: readiness fails on SIGTERM so the pod leaves the endpoints
		{"shutting down", true, nil, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if !tt.shuttingDown {
				mock.ExpectPing().WillReturnError(tt.pingErr)
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
		})
	}
}
