package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMain(m *testing.M) {
	// The subprocess in TestMainRefusesToStart runs main itself
	if os.Getenv("PAYMENT_TEST_RUN_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// The settings main exits without, as key=value. sql.Open does not connect,
// so the database URL need not reach a database.
var requiredEnv = []string{
	"DATABASE_URL=unused",
	"SERVICEBUS_QUEUE=order-events",
	"SERVICEBUS_CONNECTION_STRING=not-a-connection-string",
}

// withoutEnv returns requiredEnv less the entry for key. An empty key keeps
// every entry.
func withoutEnv(key string) []string {
	var env []string
	for _, kv := range requiredEnv {
		if !strings.HasPrefix(kv, key+"=") {
			env = append(env, kv)
		}
	}
	return env
}

// Pins #18: the service exits on startup when the database or Service Bus is
// not configured, rather than only logging the events it should publish.
func TestMainRefusesToStart(t *testing.T) {
	type test struct {
		name    string
		env     []string
		wantLog string
	}
	tests := []test{
		{"DATABASE_URL unset", withoutEnv("DATABASE_URL"), "DATABASE_URL is required"},
		{"SERVICEBUS_QUEUE unset", withoutEnv("SERVICEBUS_QUEUE"), "SERVICEBUS_QUEUE is required"},
		{"no Service Bus connection", withoutEnv("SERVICEBUS_CONNECTION_STRING"), "SERVICEBUS_CONNECTION_STRING or SERVICEBUS_NAMESPACE is required"},
		// With every setting, main gets past the checks and stops at the bad
		// connection string, before it waits for the database
		{"all set", withoutEnv(""), "Failed to create Service Bus client"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// If main ever starts serving instead, the timeout kills it and the test fails
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
			for _, kv := range os.Environ() {
				key, _, _ := strings.Cut(kv, "=")
				if key != "DATABASE_URL" && !strings.HasPrefix(key, "SERVICEBUS_") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			cmd.Env = append(cmd.Env, "PAYMENT_TEST_RUN_MAIN=1", "PORT=0")
			cmd.Env = append(cmd.Env, tt.env...)

			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || ctx.Err() != nil {
				t.Fatalf("main exited with %v, want exit status 1. Output:\n%s", err, out)
			}
			if !strings.Contains(string(out), tt.wantLog) {
				t.Errorf("output %q does not contain %q", out, tt.wantLog)
			}
		})
	}
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

// captureLog collects log output for the rest of the test. With no Service
// Bus sender, publishEvent logs each event, so this shows what was published.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	return &buf
}

func fixCharge(t *testing.T, succeeds bool) {
	t.Helper()
	orig := chargeSucceeds
	chargeSucceeds = func() bool { return succeeds }
	t.Cleanup(func() { chargeSucceeds = orig })
}

// Pins #124: a failed ledger insert or commit fails the charge with 500 and
// publishes nothing, instead of reporting a payment that rolled back.
func TestHandleChargeFailures(t *testing.T) {
	const body = `{"order_id":7,"customer_id":"alice@example.com","amount":20,"method":"card"}`
	steps := []string{"begin", "payment", "debit", "credit", "commit"}
	for _, failing := range steps {
		t.Run(failing+" fails", func(t *testing.T) {
			fixCharge(t, true)
			// Every step after the failing one is set up to succeed, so a
			// handler that carried on past the error would return 201. Those
			// steps go unused, so this test does not check that all
			// expectations were met.
			conn, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			db = conn
			errAt := func(step string) error {
				if step == failing {
					return errors.New("db error")
				}
				return nil
			}
			ok := sqlmock.NewResult(1, 1)
			mock.ExpectBegin().WillReturnError(errAt("begin"))
			mock.ExpectExec(sqlText("INSERT INTO payments")).WillReturnResult(ok).WillReturnError(errAt("payment"))
			mock.ExpectExec(sqlText("'charge'")).WillReturnResult(ok).WillReturnError(errAt("debit"))
			mock.ExpectExec(sqlText("'revenue'")).WillReturnResult(ok).WillReturnError(errAt("credit"))
			mock.ExpectCommit().WillReturnError(errAt("commit"))
			logs := captureLog(t)

			rec := httptest.NewRecorder()
			handleCharge(rec, asService(httptest.NewRequest(http.MethodPost, "/charge", strings.NewReader(body))))

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500: %s", rec.Code, rec.Body)
			}
			if strings.Contains(logs.String(), "payment.") {
				t.Errorf("published an event for a failed charge:\n%s", logs)
			}
		})
	}
}

func TestHandleCharge(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		body      string
		succeeds  bool
		want      int
		wantEvent string
	}{
		{name: "wrong method", method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "invalid json", method: http.MethodPost, body: `{`, want: http.StatusBadRequest},
		{name: "zero amount", method: http.MethodPost, body: `{"order_id":7,"amount":0}`, want: http.StatusBadRequest},
		{name: "negative amount", method: http.MethodPost, body: `{"order_id":7,"amount":-5}`, want: http.StatusBadRequest},
		{name: "completed", method: http.MethodPost, body: `{"order_id":7,"customer_id":"alice@example.com","amount":20}`, succeeds: true, want: http.StatusCreated, wantEvent: "payment.completed"},
		{name: "declined", method: http.MethodPost, body: `{"order_id":7,"customer_id":"alice@example.com","amount":20}`, succeeds: false, want: http.StatusPaymentRequired, wantEvent: "payment.failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixCharge(t, tt.succeeds)
			mock := mockDB(t)
			if tt.wantEvent != "" {
				status := "failed"
				if tt.succeeds {
					status = "completed"
				}
				mock.ExpectBegin()
				mock.ExpectExec(sqlText("INSERT INTO payments")).
					WithArgs(sqlmock.AnyArg(), 7, "alice@example.com", 20.0, "GBP", status, "").
					WillReturnResult(sqlmock.NewResult(1, 1))
				if tt.succeeds {
					// Double entry: a debit and a matching credit
					mock.ExpectExec(sqlText("'charge'")).WithArgs(sqlmock.AnyArg(), 20.0, "GBP", "Order #7 payment").
						WillReturnResult(sqlmock.NewResult(1, 1))
					mock.ExpectExec(sqlText("'revenue'")).WithArgs(sqlmock.AnyArg(), 20.0, "GBP", "Order #7 revenue").
						WillReturnResult(sqlmock.NewResult(1, 1))
				}
				mock.ExpectCommit()
			}
			logs := captureLog(t)

			rec := httptest.NewRecorder()
			handleCharge(rec, asService(httptest.NewRequest(tt.method, "/charge", strings.NewReader(tt.body))))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.wantEvent == "" {
				return
			}
			if !strings.Contains(logs.String(), tt.wantEvent) {
				t.Errorf("no %s event in logs:\n%s", tt.wantEvent, logs)
			}
			var got map[string]interface{}
			json.NewDecoder(rec.Body).Decode(&got)
			if id, _ := got["payment_id"].(string); !strings.HasPrefix(id, "pay_") {
				t.Errorf("payment_id = %v", got["payment_id"])
			}
		})
	}
}

// Pins the refund fix: the payment row is locked with FOR UPDATE inside the
// transaction, so concurrent refunds cannot both pass the status check. A
// partially refunded payment can be refunded again up to what is left, and a
// failed ledger insert or status update fails the refund.
func TestHandleRefund(t *testing.T) {
	paymentCols := []string{"amount", "order_id", "status", "customer_id", "currency", "method"}
	tests := []struct {
		name       string
		body       string
		status     string  // original payment status; empty means not found
		method     string  // original payment method; "refund" for a refund row
		refunded   float64 // already refunded by earlier refunds
		lookupErr  error
		ledgerErr  error
		updateErr  error
		wantAmount float64
		wantMark   string // status the original payment moves to
		want       int
	}{
		{name: "invalid json", body: `{`, want: http.StatusBadRequest},
		{name: "unknown payment", body: `{"payment_id":"pay_1"}`, want: http.StatusNotFound},
		{name: "lookup fails", body: `{"payment_id":"pay_1"}`, lookupErr: errors.New("db down"), want: http.StatusInternalServerError},
		{name: "payment not completed", body: `{"payment_id":"pay_1"}`, status: "failed", want: http.StatusConflict},
		{name: "already refunded", body: `{"payment_id":"pay_1"}`, status: "refunded", want: http.StatusConflict},
		{name: "a refund cannot be refunded", body: `{"payment_id":"pay_1"}`, status: "completed", method: "refund", want: http.StatusConflict},
		{name: "full refund by default", body: `{"payment_id":"pay_1"}`, status: "completed", wantAmount: 50, wantMark: "refunded", want: http.StatusOK},
		{name: "over-refund is capped", body: `{"payment_id":"pay_1","amount":80}`, status: "completed", wantAmount: 50, wantMark: "refunded", want: http.StatusOK},
		{name: "partial refund", body: `{"payment_id":"pay_1","amount":20}`, status: "completed", wantAmount: 20, wantMark: "partially_refunded", want: http.StatusOK},
		{name: "second partial refund", body: `{"payment_id":"pay_1","amount":10}`, status: "partially_refunded", refunded: 20, wantAmount: 10, wantMark: "partially_refunded", want: http.StatusOK},
		{name: "rest of a partial refund", body: `{"payment_id":"pay_1"}`, status: "partially_refunded", refunded: 20.1, wantAmount: 29.9, wantMark: "refunded", want: http.StatusOK},
		{name: "partial refund capped at what is left", body: `{"payment_id":"pay_1","amount":45}`, status: "partially_refunded", refunded: 20, wantAmount: 30, wantMark: "refunded", want: http.StatusOK},
		{name: "nothing left", body: `{"payment_id":"pay_1"}`, status: "partially_refunded", refunded: 50, want: http.StatusConflict},
		// Amounts are rounded to pence before the comparison, so the last
		// refund marks the payment refunded instead of leaving a fraction
		{name: "sub-penny amount is rounded", body: `{"payment_id":"pay_1","amount":29.8999}`, status: "partially_refunded", refunded: 20.1, wantAmount: 29.9, wantMark: "refunded", want: http.StatusOK},
		{name: "amount rounded to pence", body: `{"payment_id":"pay_1","amount":10.006}`, status: "completed", wantAmount: 10.01, wantMark: "partially_refunded", want: http.StatusOK},
		{name: "ledger insert fails", body: `{"payment_id":"pay_1"}`, status: "completed", ledgerErr: errors.New("db down"), wantAmount: 50, want: http.StatusInternalServerError},
		{name: "status update fails", body: `{"payment_id":"pay_1"}`, status: "completed", updateErr: errors.New("db down"), wantAmount: 50, wantMark: "refunded", want: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if tt.body != `{` {
				mock.ExpectBegin()
				q := mock.ExpectQuery(sqlText("FROM payments WHERE id = $1 FOR UPDATE")).WithArgs("pay_1")
				switch {
				case tt.lookupErr != nil:
					q.WillReturnError(tt.lookupErr)
				case tt.status == "":
					q.WillReturnError(sql.ErrNoRows)
				default:
					var method interface{}
					if tt.method != "" {
						method = tt.method
					}
					q.WillReturnRows(sqlmock.NewRows(paymentCols).AddRow(50.0, 7, tt.status, "alice@example.com", "GBP", method))
				}
				if (tt.status == "completed" || tt.status == "partially_refunded") && tt.method == "" {
					mock.ExpectQuery(sqlText("WHERE reference = $1 AND method = 'refund'")).WithArgs("pay_1").
						WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(tt.refunded))
				}
			}
			if tt.wantAmount > 0 {
				mock.ExpectExec(sqlText("INSERT INTO payments")).
					WithArgs(sqlmock.AnyArg(), 7, "alice@example.com", tt.wantAmount, "GBP", "pay_1").
					WillReturnResult(sqlmock.NewResult(1, 1))
				l := mock.ExpectExec(sqlText("INSERT INTO ledger_entries")).WithArgs(sqlmock.AnyArg(), tt.wantAmount, "GBP", sqlmock.AnyArg())
				if tt.ledgerErr != nil {
					l.WillReturnError(tt.ledgerErr)
				} else {
					l.WillReturnResult(sqlmock.NewResult(1, 1))
					u := mock.ExpectExec(sqlText("UPDATE payments SET status = $1")).WithArgs(tt.wantMark, "pay_1")
					if tt.updateErr != nil {
						u.WillReturnError(tt.updateErr)
					} else {
						u.WillReturnResult(sqlmock.NewResult(0, 1))
						mock.ExpectCommit()
					}
				}
			}
			if tt.body != `{` && tt.want != http.StatusOK {
				mock.ExpectRollback()
			}

			rec := httptest.NewRecorder()
			handleRefund(rec, asService(httptest.NewRequest(http.MethodPost, "/refund", strings.NewReader(tt.body))))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want == http.StatusOK {
				var got struct {
					Amount float64 `json:"amount"`
				}
				json.NewDecoder(rec.Body).Decode(&got)
				if got.Amount != tt.wantAmount {
					t.Errorf("amount = %v, want %v", got.Amount, tt.wantAmount)
				}
			}
		})
	}
}

func TestHandleRefundTooSmall(t *testing.T) {
	mockDB(t) // any SQL fails the test
	rec := httptest.NewRecorder()
	handleRefund(rec, asService(httptest.NewRequest(http.MethodPost, "/refund", strings.NewReader(`{"payment_id":"pay_1","amount":0.004}`))))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestHandleBalanceQueryErrors(t *testing.T) {
	for _, failing := range []int{1, 2} {
		t.Run(fmt.Sprintf("query %d fails", failing), func(t *testing.T) {
			mock := mockDB(t)
			q := mock.ExpectQuery(sqlText("method != 'refund'")).WithArgs("alice@example.com")
			if failing == 1 {
				q.WillReturnError(errors.New("db down"))
			} else {
				q.WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(10.0))
				mock.ExpectQuery(sqlText("AND method = 'refund'")).WithArgs("alice@example.com").WillReturnError(errors.New("db down"))
			}
			req := httptest.NewRequest(http.MethodGet, "/balance/alice@example.com", nil)
			req.Header.Set("X-User-Email", "alice@example.com")
			rec := httptest.NewRecorder()
			handleBalance(rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", rec.Code)
			}
		})
	}
}

func TestHandleBalance(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		role     string
		charged  float64
		refunded float64
		want     int
	}{
		{name: "no customer", path: "/balance/", want: http.StatusBadRequest},
		{name: "net of refunds", path: "/balance/alice@example.com", charged: 100, refunded: 30, want: http.StatusOK},
		// The caller is alice. Customers see only their own balance.
		{name: "another customer's balance", path: "/balance/bob@example.com", role: "customer", want: http.StatusForbidden},
		{name: "admin reads another balance", path: "/balance/alice@example.com", role: "admin", charged: 5, want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if tt.want == http.StatusOK {
				mock.ExpectQuery(sqlText("method != 'refund'")).WithArgs("alice@example.com").
					WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(tt.charged))
				mock.ExpectQuery(sqlText("AND method = 'refund'")).WithArgs("alice@example.com").
					WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(tt.refunded))
			}

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("X-User-Email", "alice@example.com")
			req.Header.Set("X-User-Role", tt.role)
			if tt.role == "admin" {
				req.Header.Set("X-User-Email", "root@example.com")
			}
			rec := httptest.NewRecorder()
			handleBalance(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.want == http.StatusOK {
				var got map[string]interface{}
				json.NewDecoder(rec.Body).Decode(&got)
				if got["net"] != tt.charged-tt.refunded {
					t.Errorf("net = %v, want %v", got["net"], tt.charged-tt.refunded)
				}
			}
		})
	}
}

// Customers see the ledger entries for their own payments only. Admin and
// service callers see all of them.
func TestHandleLedger(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		wantSQL  string
		wantArgs []driver.Value
	}{
		{"customer", "customer", "FROM ledger_entries le JOIN payments p ON p.id = le.payment_id WHERE p.customer_id = $1 ORDER BY", []driver.Value{"alice@example.com"}},
		{"no role is a customer", "", "WHERE p.customer_id = $1 ORDER BY", []driver.Value{"alice@example.com"}},
		{"admin", "admin", "FROM ledger_entries le ORDER BY", nil},
		{"service", "service", "FROM ledger_entries le ORDER BY", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			mock.ExpectQuery(sqlText(tt.wantSQL)).WithArgs(tt.wantArgs...).WillReturnRows(
				sqlmock.NewRows([]string{"id", "payment_id", "entry_type", "debit", "credit", "currency", "description", "created_at"}).
					AddRow(1, "pay_1", "charge", 20.0, 0.0, "GBP", "Order #7 payment", "t").
					AddRow(2, "pay_1", "revenue", 0.0, 20.0, "GBP", "Order #7 revenue", "t"),
			)

			req := httptest.NewRequest(http.MethodGet, "/ledger", nil)
			req.Header.Set("X-User-Email", "alice@example.com")
			req.Header.Set("X-User-Role", tt.role)
			rec := httptest.NewRecorder()
			handleLedger(rec, req)

			var entries []map[string]interface{}
			json.NewDecoder(rec.Body).Decode(&entries)
			if rec.Code != http.StatusOK || len(entries) != 2 {
				t.Errorf("status = %d, entries = %v", rec.Code, entries)
			}
		})
	}
}

// Customers cannot charge or refund. Only internal callers and admins can.
func TestChargeAndRefundNeedPrivilege(t *testing.T) {
	handlers := map[string]http.HandlerFunc{"/charge": handleCharge, "/refund": handleRefund}
	for path, h := range handlers {
		for _, role := range []string{"", "customer"} {
			t.Run(path+" role="+role, func(t *testing.T) {
				mockDB(t) // any SQL fails the test
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"order_id":7,"customer_id":"bob@example.com","amount":5,"payment_id":"pay_1"}`))
				req.Header.Set("X-User-Email", "alice@example.com")
				req.Header.Set("X-User-Role", role)
				rec := httptest.NewRecorder()
				h(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Errorf("status = %d, want 403", rec.Code)
				}
			})
		}
	}
}

func asService(req *http.Request) *http.Request {
	req.Header.Set("X-User-Role", "service")
	return req
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
		{"/charge", "", http.StatusUnauthorized},
		{"/refund", "", http.StatusUnauthorized},
		{"/ledger", "", http.StatusUnauthorized},
		{"/balance/alice@example.com", "", http.StatusUnauthorized},
		{"/charge", "alice@example.com", http.StatusOK},
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
