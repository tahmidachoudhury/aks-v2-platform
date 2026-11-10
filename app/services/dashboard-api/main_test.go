package main

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
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

func one(v interface{}) *sqlmock.Rows { return sqlmock.NewRows([]string{"v"}).AddRow(v) }

// Pins #129: the dashboard data needs X-User-Email, which only the gateway
// sets after checking the JWT, and role admin. The UI page and health checks
// stay open.
func TestRequireAdmin(t *testing.T) {
	tests := []struct {
		path string
		user string
		role string
		want int
	}{
		{"/dashboard/summary", "", "", http.StatusUnauthorized},
		{"/dashboard/orders/stats", "", "", http.StatusUnauthorized},
		{"/dashboard/revenue", "", "", http.StatusUnauthorized},
		{"/dashboard/inventory/alerts", "", "", http.StatusUnauthorized},
		{"/dashboard/shipping/overview", "", "", http.StatusUnauthorized},
		{"/dashboard/anything-new", "", "", http.StatusUnauthorized},
		{"/dashboard/summary", "root@example.com", "admin", http.StatusOK},
		{"/dashboard/revenue", "root@example.com", "admin", http.StatusOK},
		{"/dashboard/summary", "alice@example.com", "customer", http.StatusForbidden},
		{"/dashboard/summary", "alice@example.com", "", http.StatusForbidden},
		{"/dashboard/summary", "scheduler", "service", http.StatusForbidden},
		{"/dashboard/healthz", "", "", http.StatusOK},
		{"/healthz", "", "", http.StatusOK},
		{"/livez", "", "", http.StatusOK},
		{"/", "", "", http.StatusOK},
		{"/index.html", "", "", http.StatusOK},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, tt := range tests {
		t.Run(tt.path+" user="+tt.user+" role="+tt.role, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.user != "" {
				req.Header.Set("X-User-Email", tt.user)
			}
			req.Header.Set("X-User-Role", tt.role)
			rec := httptest.NewRecorder()
			requireAdmin(next).ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestStaticUIEmbedded(t *testing.T) {
	page, err := fs.ReadFile(staticFiles, "static/index.html")
	if err != nil || len(page) == 0 {
		t.Fatalf("static/index.html not embedded: %v", err)
	}
}

func TestHandleSummary(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("SELECT COUNT(*) FROM orders")).WithoutArgs().WillReturnRows(one(12))
	mock.ExpectQuery(sqlText("FROM orders WHERE created_at >= $1")).WillReturnRows(one(3))
	mock.ExpectQuery(sqlText("SELECT status, COUNT(*) FROM orders GROUP BY status")).WillReturnRows(
		sqlmock.NewRows([]string{"status", "count"}).AddRow("pending", 2).AddRow("delivered", 10))
	mock.ExpectQuery(sqlText("(method IS NULL OR method != 'refund')")).WithoutArgs().WillReturnRows(one(500.0))
	mock.ExpectQuery(sqlText("AND method = 'refund'")).WithoutArgs().WillReturnRows(one(120.0))
	mock.ExpectQuery(sqlText("(method IS NULL OR method != 'refund') AND created_at >= $1")).WillReturnRows(one(80.0))
	mock.ExpectQuery(sqlText("AND method = 'refund' AND created_at >= $1")).WillReturnRows(one(30.0))
	mock.ExpectQuery(sqlText("SELECT COUNT(*) FROM products")).WithoutArgs().WillReturnRows(one(40))
	mock.ExpectQuery(sqlText("WHERE (stock - reserved) < 10")).WillReturnRows(one(4))
	mock.ExpectQuery(sqlText("FROM shipments WHERE status NOT IN ('delivered', 'cancelled')")).WillReturnRows(one(6))

	rec := httptest.NewRecorder()
	handleSummary(rec, httptest.NewRequest(http.MethodGet, "/dashboard/summary", nil))

	var got struct {
		Orders struct {
			Total    int            `json:"total"`
			Today    int            `json:"today"`
			ByStatus map[string]int `json:"by_status"`
		} `json:"orders"`
		Revenue struct {
			Total float64 `json:"total"`
			Today float64 `json:"today"`
		} `json:"revenue"`
		Inventory struct {
			TotalProducts int `json:"total_products"`
			LowStock      int `json:"low_stock"`
		} `json:"inventory"`
		Shipping struct {
			Active int `json:"active_shipments"`
		} `json:"shipping"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	checks := []struct {
		name      string
		got, want interface{}
	}{
		{"orders.total", got.Orders.Total, 12},
		{"orders.today", got.Orders.Today, 3},
		{"orders.by_status[delivered]", got.Orders.ByStatus["delivered"], 10},
		{"revenue.total is charges minus refunds", got.Revenue.Total, 380.0},
		{"revenue.today is charges minus refunds", got.Revenue.Today, 50.0},
		{"inventory.total_products", got.Inventory.TotalProducts, 40},
		{"inventory.low_stock", got.Inventory.LowStock, 4},
		{"shipping.active_shipments", got.Shipping.Active, 6},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestHandleRevenue(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("(method IS NULL OR method != 'refund')")).WillReturnRows(one(200.0))
	mock.ExpectQuery(sqlText("AND method = 'refund'")).WillReturnRows(one(45.5))
	mock.ExpectQuery(sqlText("INTERVAL '7 days'")).WillReturnRows(
		sqlmock.NewRows([]string{"day", "sum"}).AddRow("2026-11-08", 120.0).AddRow("2026-11-07", 80.0))

	rec := httptest.NewRecorder()
	handleRevenue(rec, httptest.NewRequest(http.MethodGet, "/dashboard/revenue", nil))

	var got struct {
		Total    float64           `json:"total"`
		Refunded float64           `json:"refunded"`
		Net      float64           `json:"net"`
		Daily    []json.RawMessage `json:"daily"`
	}
	json.NewDecoder(rec.Body).Decode(&got)
	if got.Total != 200 || got.Refunded != 45.5 || got.Net != 154.5 || len(got.Daily) != 2 {
		t.Errorf("revenue = %+v", got)
	}
}

func TestListHandlers(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		sql     string
		rows    *sqlmock.Rows
		err     error
		want    int
		count   int
	}{
		{
			name: "order stats", handler: handleOrderStats, sql: "INTERVAL '30 days'",
			rows: sqlmock.NewRows([]string{"day", "count", "sum"}).AddRow("2026-11-08", 4, 99.5),
			want: http.StatusOK, count: 1,
		},
		{name: "order stats fail", handler: handleOrderStats, sql: "FROM orders", err: errors.New("db down"), want: http.StatusInternalServerError},
		{
			name: "inventory alerts", handler: handleInventoryAlerts, sql: "WHERE (stock - reserved) < 10",
			rows: sqlmock.NewRows([]string{"id", "name", "sku", "stock", "reserved", "available"}).
				AddRow("p1", "Mug", "MUG-1", 5, 2, 3).AddRow("p2", "Cup", "CUP-1", 1, 1, 0),
			want: http.StatusOK, count: 2,
		},
		{name: "inventory alerts fail", handler: handleInventoryAlerts, sql: "FROM products", err: errors.New("db down"), want: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			q := mock.ExpectQuery(sqlText(tt.sql))
			if tt.err != nil {
				q.WillReturnError(tt.err)
			} else {
				q.WillReturnRows(tt.rows)
			}

			rec := httptest.NewRecorder()
			tt.handler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.err == nil {
				var got []json.RawMessage
				json.NewDecoder(rec.Body).Decode(&got)
				if len(got) != tt.count {
					t.Errorf("got %d items, want %d", len(got), tt.count)
				}
			}
		})
	}
}

func TestHandleShippingOverview(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("SELECT status, COUNT(*) FROM shipments GROUP BY status")).WillReturnRows(
		sqlmock.NewRows([]string{"status", "count"}).AddRow("in_transit", 3).AddRow("delivered", 5))
	mock.ExpectQuery(sqlText("SELECT carrier, COUNT(*) FROM shipments GROUP BY carrier")).WillReturnRows(
		sqlmock.NewRows([]string{"carrier", "count"}).AddRow("dpd", 8))
	mock.ExpectQuery(sqlText("AVG(EXTRACT(EPOCH FROM (delivered_at - shipped_at))")).WillReturnRows(one(36.0))

	rec := httptest.NewRecorder()
	handleShippingOverview(rec, httptest.NewRequest(http.MethodGet, "/dashboard/shipping/overview", nil))

	var got struct {
		ByStatus  map[string]int `json:"by_status"`
		ByCarrier map[string]int `json:"by_carrier"`
		AvgHours  float64        `json:"avg_delivery_hours"`
	}
	json.NewDecoder(rec.Body).Decode(&got)
	if got.ByStatus["delivered"] != 5 || got.ByCarrier["dpd"] != 8 || got.AvgHours != 36 {
		t.Errorf("overview = %+v", got)
	}
}

// A failed query, scan or row returns 500 instead of zeros with a 200. The
// handler stops at the first failure, so sqlmock fails any later query.
func TestHandlersFailOnQueryError(t *testing.T) {
	dbDown := errors.New("db down")
	tests := []struct {
		name    string
		handler http.HandlerFunc
		setup   func(mock sqlmock.Sqlmock)
	}{
		{"summary, first count", handleSummary, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("SELECT COUNT(*) FROM orders")).WillReturnError(dbDown)
		}},
		{"summary, status counts scan", handleSummary, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("SELECT COUNT(*) FROM orders")).WillReturnRows(one(12))
			mock.ExpectQuery(sqlText("FROM orders WHERE created_at >= $1")).WillReturnRows(one(3))
			mock.ExpectQuery(sqlText("GROUP BY status")).WillReturnRows(
				sqlmock.NewRows([]string{"status", "count"}).AddRow("pending", "many"))
		}},
		{"summary, last count", handleSummary, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("SELECT COUNT(*) FROM orders")).WillReturnRows(one(12))
			mock.ExpectQuery(sqlText("FROM orders WHERE created_at >= $1")).WillReturnRows(one(3))
			mock.ExpectQuery(sqlText("GROUP BY status")).WillReturnRows(sqlmock.NewRows([]string{"status", "count"}))
			for i := 0; i < 6; i++ {
				mock.ExpectQuery(".").WillReturnRows(one(1))
			}
			mock.ExpectQuery(sqlText("FROM shipments")).WillReturnError(dbDown)
		}},
		{"revenue, total", handleRevenue, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("(method IS NULL OR method != 'refund')")).WillReturnError(dbDown)
		}},
		{"revenue, daily", handleRevenue, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("(method IS NULL OR method != 'refund')")).WillReturnRows(one(200.0))
			mock.ExpectQuery(sqlText("AND method = 'refund'")).WillReturnRows(one(45.5))
			mock.ExpectQuery(sqlText("INTERVAL '7 days'")).WillReturnError(dbDown)
		}},
		{"order stats, row error", handleOrderStats, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("INTERVAL '30 days'")).WillReturnRows(
				sqlmock.NewRows([]string{"day", "count", "sum"}).AddRow("2026-11-08", 4, 99.5).RowError(0, dbDown))
		}},
		{"inventory alerts, scan", handleInventoryAlerts, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("WHERE (stock - reserved) < 10")).WillReturnRows(
				sqlmock.NewRows([]string{"id", "name", "sku", "stock", "reserved", "available"}).AddRow("p1", "Mug", "MUG-1", "five", 2, 3))
		}},
		{"shipping overview, carriers", handleShippingOverview, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("GROUP BY status")).WillReturnRows(sqlmock.NewRows([]string{"status", "count"}))
			mock.ExpectQuery(sqlText("GROUP BY carrier")).WillReturnError(dbDown)
		}},
		{"shipping overview, average", handleShippingOverview, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("GROUP BY status")).WillReturnRows(sqlmock.NewRows([]string{"status", "count"}))
			mock.ExpectQuery(sqlText("GROUP BY carrier")).WillReturnRows(sqlmock.NewRows([]string{"carrier", "count"}))
			mock.ExpectQuery(sqlText("AVG(EXTRACT")).WillReturnError(dbDown)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			tt.setup(mock)
			rec := httptest.NewRecorder()
			tt.handler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500: %s", rec.Code, rec.Body)
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
