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
		name        string
		method      string
		body        string
		template    bool // the template exists in the database
		wantChannel string
		wantSubject string
		wantBody    string
		want        int
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
			name: "unknown template falls back to its name", method: http.MethodPost,
			body:        `{"recipient":"+447700900000","channel":"sms","template":"custom_text"}`,
			wantChannel: "sms", wantSubject: "custom_text", wantBody: "custom_text", want: http.StatusCreated,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if tt.want == http.StatusCreated {
				var req struct {
					Recipient string `json:"recipient"`
					Template  string `json:"template"`
				}
				json.Unmarshal([]byte(tt.body), &req)
				q := mock.ExpectQuery(sqlText("FROM notification_templates WHERE id = $1")).WithArgs(req.Template)
				if tt.template {
					q.WillReturnRows(sqlmock.NewRows([]string{"subject", "body"}).AddRow(tt.wantSubject, tt.wantBody))
				} else {
					q.WillReturnError(sql.ErrNoRows)
				}
				mock.ExpectQuery(sqlText("INSERT INTO notifications")).
					WithArgs(req.Recipient, tt.wantChannel, req.Template, tt.wantSubject, tt.wantBody, sqlmock.AnyArg(), "sent", sqlmock.AnyArg()).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(5))
			}

			rec := httptest.NewRecorder()
			handleSend(rec, httptest.NewRequest(tt.method, "/send", strings.NewReader(tt.body)))

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
	tests := []struct {
		name     string
		url      string
		wantSQL  string
		wantArgs []driver.Value
	}{
		{"everyone's", "/history", "FROM notifications ORDER BY created_at DESC", nil},
		{"one recipient", "/history?recipient=alice@example.com", "FROM notifications WHERE recipient = $1 ORDER BY", []driver.Value{"alice@example.com"}},
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

			rec := httptest.NewRecorder()
			handleHistory(rec, httptest.NewRequest(http.MethodGet, tt.url, nil))

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
		})
	}
}
