package main

import (
	"database/sql"
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

var productColumns = []string{"id", "name", "sku", "price", "stock", "reserved", "created_at", "updated_at"}

func expectStock(mock sqlmock.Sqlmock, productID string, stock, reserved int) {
	mock.ExpectQuery(sqlText("SELECT stock, reserved FROM products WHERE id = $1 FOR UPDATE")).
		WithArgs(productID).
		WillReturnRows(sqlmock.NewRows([]string{"stock", "reserved"}).AddRow(stock, reserved))
}

func TestHandleReserve(t *testing.T) {
	const twoItems = `{"order_id":7,"items":[{"product_id":"p1","quantity":2},{"product_id":"p2","quantity":1}]}`
	tests := []struct {
		name   string
		method string
		body   string
		setup  func(mock sqlmock.Sqlmock)
		want   int
	}{
		{name: "wrong method", method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "invalid json", method: http.MethodPost, body: `{`, want: http.StatusBadRequest},
		{
			name: "reserves every item", method: http.MethodPost, body: twoItems, want: http.StatusCreated,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				expectStock(mock, "p1", 10, 3)
				mock.ExpectExec(sqlText("UPDATE products SET reserved = reserved + $1")).WithArgs(2, "p1").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(sqlText("INSERT INTO reservations")).WithArgs(7, "p1", 2, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
				expectStock(mock, "p2", 1, 0)
				mock.ExpectExec(sqlText("UPDATE products SET reserved = reserved + $1")).WithArgs(1, "p2").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(sqlText("INSERT INTO reservations")).WithArgs(7, "p2", 1, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectCommit()
			},
		},
		{
			name: "unknown product", method: http.MethodPost, body: twoItems, want: http.StatusNotFound,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(sqlText("FOR UPDATE")).WithArgs("p1").WillReturnError(sql.ErrNoRows)
				mock.ExpectRollback()
			},
		},
		{
			// Available is stock minus what is already reserved: 10 - 9 = 1 < 2
			name: "not enough available stock", method: http.MethodPost, body: twoItems, want: http.StatusConflict,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				expectStock(mock, "p1", 10, 9)
				mock.ExpectRollback()
			},
		},
		{
			name: "second item short rolls back the first", method: http.MethodPost, body: twoItems, want: http.StatusConflict,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				expectStock(mock, "p1", 10, 0)
				mock.ExpectExec(sqlText("UPDATE products")).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(sqlText("INSERT INTO reservations")).WillReturnResult(sqlmock.NewResult(1, 1))
				expectStock(mock, "p2", 0, 0)
				mock.ExpectRollback()
			},
		},
		{
			name: "update fails", method: http.MethodPost, body: twoItems, want: http.StatusInternalServerError,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				expectStock(mock, "p1", 10, 0)
				mock.ExpectExec(sqlText("UPDATE products")).WillReturnError(errors.New("db down"))
				mock.ExpectRollback()
			},
		},
		{
			name: "commit fails", method: http.MethodPost, body: `{"order_id":7,"items":[{"product_id":"p1","quantity":1}]}`, want: http.StatusInternalServerError,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				expectStock(mock, "p1", 10, 0)
				mock.ExpectExec(sqlText("UPDATE products")).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(sqlText("INSERT INTO reservations")).WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectCommit().WillReturnError(errors.New("serialization failure"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if tt.setup != nil {
				tt.setup(mock)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, "/reserve", strings.NewReader(tt.body))
			req.Header.Set("X-User-Role", "service")
			handleReserve(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// Customers cannot reserve or release stock. Only internal callers, such as
// the scheduler, and admins can.
func TestReserveAndReleaseNeedPrivilege(t *testing.T) {
	handlers := map[string]http.HandlerFunc{"/reserve": handleReserve, "/release": handleRelease}
	for path, h := range handlers {
		for _, role := range []string{"", "customer"} {
			t.Run(path+" role="+role, func(t *testing.T) {
				mockDB(t) // any SQL fails the test
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"order_id":7}`))
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

// /release claims the order's active reservations with a conditional UPDATE
// before returning stock, and returns only what it claimed. A reservation the
// scheduler has already expired is not claimed, so its stock is not returned
// twice.
func TestHandleRelease(t *testing.T) {
	claim := sqlText("UPDATE reservations SET status = 'released'")
	release := sqlText("UPDATE products SET reserved = GREATEST(reserved - $1, 0)")
	claimed := func() *sqlmock.Rows { return sqlmock.NewRows([]string{"product_id", "quantity"}) }
	tests := []struct {
		name         string
		method       string
		body         string
		setup        func(mock sqlmock.Sqlmock)
		want         int
		wantReleased int
	}{
		{name: "wrong method", method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "invalid json", method: http.MethodPost, body: `{`, want: http.StatusBadRequest},
		{
			name: "releases each claimed reservation", method: http.MethodPost, body: `{"order_id":7}`, want: http.StatusOK, wantReleased: 2,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(claim + ".*" + sqlText("WHERE order_id = $1 AND status = 'active'") + ".*RETURNING product_id, quantity").WithArgs(7).
					WillReturnRows(claimed().AddRow("p1", 2).AddRow("p2", 1))
				mock.ExpectExec(release).WithArgs(2, "p1").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(release).WithArgs(1, "p2").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			},
		},
		{
			// The scheduler expired every row first: nothing is claimed, so
			// no stock is returned a second time
			name: "already expired by the scheduler", method: http.MethodPost, body: `{"order_id":7}`, want: http.StatusOK, wantReleased: 0,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(claim).WithArgs(7).WillReturnRows(claimed())
				mock.ExpectCommit()
			},
		},
		{
			name: "claim fails", method: http.MethodPost, body: `{"order_id":7}`, want: http.StatusInternalServerError,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(claim).WithArgs(7).WillReturnError(errors.New("lock timeout"))
				mock.ExpectRollback()
			},
		},
		{
			name: "stock update fails", method: http.MethodPost, body: `{"order_id":7}`, want: http.StatusInternalServerError,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(claim).WithArgs(7).WillReturnRows(claimed().AddRow("p1", 2))
				mock.ExpectExec(release).WillReturnError(errors.New("db down"))
				mock.ExpectRollback()
			},
		},
		{
			name: "commit fails", method: http.MethodPost, body: `{"order_id":7}`, want: http.StatusInternalServerError,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(claim).WithArgs(7).WillReturnRows(claimed())
				mock.ExpectCommit().WillReturnError(errors.New("serialization failure"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if tt.setup != nil {
				tt.setup(mock)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, "/release", strings.NewReader(tt.body))
			req.Header.Set("X-User-Role", "service")
			handleRelease(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want == http.StatusOK {
				var got struct {
					Released int `json:"released"`
				}
				json.NewDecoder(rec.Body).Decode(&got)
				if got.Released != tt.wantReleased {
					t.Errorf("released = %d, want %d", got.Released, tt.wantReleased)
				}
			}
		})
	}
}

func TestHandleProducts(t *testing.T) {
	tests := []struct {
		name   string
		method string
		body   string
		role   string // default admin
		setup  func(mock sqlmock.Sqlmock)
		want   int
	}{
		{
			name: "list", method: http.MethodGet, want: http.StatusOK,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(sqlText("FROM products ORDER BY name")).WillReturnRows(
					sqlmock.NewRows(productColumns).AddRow("p1", "Mug", "MUG-1", 8.5, 10, 4, "t", "t"))
			},
		},
		{
			name: "list fails", method: http.MethodGet, want: http.StatusInternalServerError,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(sqlText("FROM products")).WillReturnError(errors.New("db down"))
			},
		},
		{name: "create with invalid json", method: http.MethodPost, body: `{`, want: http.StatusBadRequest},
		{
			name: "create", method: http.MethodPost, body: `{"id":"p1","name":"Mug","sku":"MUG-1","price":8.5,"stock":10}`, want: http.StatusCreated,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectExec(sqlText("INSERT INTO products")).WithArgs("p1", "Mug", "MUG-1", 8.5, 10).
					WillReturnResult(sqlmock.NewResult(1, 1))
			},
		},
		{
			name: "create duplicate", method: http.MethodPost, body: `{"id":"p1"}`, want: http.StatusConflict,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectExec(sqlText("INSERT INTO products")).WillReturnError(errors.New("duplicate key"))
			},
		},
		{name: "wrong method", method: http.MethodDelete, want: http.StatusMethodNotAllowed},
		// Product writes are admin only. Anyone signed in can list.
		{name: "customer cannot create", method: http.MethodPost, role: "customer", body: `{"id":"p1","name":"Mug"}`, want: http.StatusForbidden},
		{name: "service cannot create", method: http.MethodPost, role: "service", body: `{"id":"p1","name":"Mug"}`, want: http.StatusForbidden},
		{
			name: "customer can list", method: http.MethodGet, role: "customer", want: http.StatusOK,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(sqlText("FROM products ORDER BY name")).WillReturnRows(sqlmock.NewRows(productColumns))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if tt.setup != nil {
				tt.setup(mock)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, "/products", strings.NewReader(tt.body))
			role := tt.role
			if role == "" {
				role = "admin"
			}
			req.Header.Set("X-User-Role", role)
			handleProducts(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.name == "list" {
				var got []Product
				json.NewDecoder(rec.Body).Decode(&got)
				if len(got) != 1 || got[0].Available != 6 {
					t.Errorf("products = %+v, want available 6", got)
				}
			}
		})
	}
}

func TestHandleProduct(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		found         bool
		want          int
		wantAvailable int
	}{
		{name: "no id", path: "/products/", want: http.StatusBadRequest},
		{name: "unknown", path: "/products/p9", want: http.StatusNotFound},
		{name: "found", path: "/products/p1", found: true, want: http.StatusOK, wantAvailable: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			if tt.path != "/products/" {
				id := strings.TrimPrefix(tt.path, "/products/")
				q := mock.ExpectQuery(sqlText("FROM products WHERE id = $1")).WithArgs(id)
				if tt.found {
					q.WillReturnRows(sqlmock.NewRows(productColumns).AddRow("p1", "Mug", "MUG-1", 8.5, 10, 3, "t", "t"))
				} else {
					q.WillReturnError(sql.ErrNoRows)
				}
			}
			rec := httptest.NewRecorder()
			handleProduct(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.found {
				var got Product
				json.NewDecoder(rec.Body).Decode(&got)
				if got.Available != tt.wantAvailable {
					t.Errorf("available = %d, want %d", got.Available, tt.wantAvailable)
				}
			}
		})
	}
}

func TestHandleLowStock(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("WHERE (stock - reserved) < $1")).WithArgs(10).WillReturnRows(
		sqlmock.NewRows([]string{"id", "name", "sku", "stock", "reserved"}).AddRow("p1", "Mug", "MUG-1", 5, 2))

	rec := httptest.NewRecorder()
	handleLowStock(rec, httptest.NewRequest(http.MethodGet, "/low-stock", nil))

	var got []struct {
		Available int `json:"available"`
	}
	json.NewDecoder(rec.Body).Decode(&got)
	if rec.Code != http.StatusOK || len(got) != 1 || got[0].Available != 3 {
		t.Errorf("status = %d, items = %+v", rec.Code, got)
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
		{"/products", "", http.StatusUnauthorized},
		{"/products/p1", "", http.StatusUnauthorized},
		{"/reserve", "", http.StatusUnauthorized},
		{"/release", "", http.StatusUnauthorized},
		{"/low-stock", "", http.StatusUnauthorized},
		{"/release", "scheduler", http.StatusOK},
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
