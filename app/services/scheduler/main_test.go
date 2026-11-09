package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// mockDB points the package-level db at a sqlmock and checks every
// expectation was met when the test ends.
func mockDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	conn, mock, err := sqlmock.New()
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

// call is one request a fake backend received.
type call struct {
	Service string
	Method  string
	Path    string
	User    string
	Body    map[string]interface{}
}

// fakeBackends starts stand-ins for order-service and inventory-service and
// points services at them. status picks each response code by service and
// order ID. The returned function lists the calls received so far.
func fakeBackends(t *testing.T, status func(service string, orderID int) int) func() []call {
	t.Helper()
	var mu sync.Mutex
	var calls []call
	serve := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := call{Service: name, Method: r.Method, Path: r.URL.Path, User: r.Header.Get("X-User-Email")}
			json.NewDecoder(r.Body).Decode(&c.Body)
			mu.Lock()
			calls = append(calls, c)
			mu.Unlock()
			id, _ := c.Body["order_id"].(float64)
			w.WriteHeader(status(name, int(id)))
		}))
	}
	order, inventory := serve("order"), serve("inventory")
	t.Cleanup(order.Close)
	t.Cleanup(inventory.Close)

	orig := services
	services = map[string]string{"order": order.URL, "inventory": inventory.URL}
	t.Cleanup(func() { services = orig })
	return func() []call {
		mu.Lock()
		defer mu.Unlock()
		return append([]call(nil), calls...)
	}
}

// Pins #123: abandoned orders are cancelled through order-service's state
// machine, not with a direct UPDATE, and their stock is released after.
func TestDetectAbandonedOrders(t *testing.T) {
	cancel := func(id int) call {
		return call{"order", http.MethodPut, "/status", "scheduler", map[string]interface{}{"order_id": float64(id), "new_status": "cancelled"}}
	}
	release := func(id int) call {
		return call{"inventory", http.MethodPost, "/release", "scheduler", map[string]interface{}{"order_id": float64(id)}}
	}
	ok := func(string, int) int { return http.StatusOK }

	tests := []struct {
		name   string
		orders []int
		status func(service string, orderID int) int
		want   []call
	}{
		{
			name:   "cancels then releases stock",
			orders: []int{5},
			status: ok,
			want:   []call{cancel(5), release(5)},
		},
		{
			name:   "refused cancel leaves the stock reserved",
			orders: []int{5},
			status: func(s string, _ int) int {
				if s == "order" {
					return http.StatusConflict
				}
				return http.StatusOK
			},
			want: []call{cancel(5)},
		},
		{
			name:   "failed release is not retried",
			orders: []int{5},
			status: func(s string, _ int) int {
				if s == "inventory" {
					return http.StatusInternalServerError
				}
				return http.StatusOK
			},
			want: []call{cancel(5), release(5)},
		},
		{
			name:   "one failure does not stop the rest",
			orders: []int{5, 6},
			status: func(s string, id int) int {
				if s == "order" && id == 5 {
					return http.StatusBadGateway
				}
				return http.StatusOK
			},
			want: []call{cancel(5), cancel(6), release(6)},
		},
		{
			name:   "nothing abandoned",
			orders: nil,
			status: ok,
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := fakeBackends(t, tt.status)
			mock := mockDB(t)
			rows := sqlmock.NewRows([]string{"id"})
			for _, id := range tt.orders {
				rows.AddRow(id)
			}
			// Only the read is expected. sqlmock fails any other statement.
			mock.ExpectQuery(sqlText("WHERE status = 'pending' AND created_at < $1")).
				WithArgs(sqlmock.AnyArg()).WillReturnRows(rows)

			detectAbandonedOrders()

			got, _ := json.Marshal(calls())
			want, _ := json.Marshal(tt.want)
			if string(got) != string(want) {
				t.Errorf("calls:\n got  %s\n want %s", got, want)
			}
		})
	}
}

func TestDetectAbandonedOrdersBackendDown(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	orig := services
	services = map[string]string{"order": down.URL, "inventory": down.URL}
	t.Cleanup(func() { services = orig })

	mock := mockDB(t)
	mock.ExpectQuery(sqlText("WHERE status = 'pending'")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(5))

	// Must log and move on, not panic or block
	detectAbandonedOrders()
}

func TestDetectAbandonedOrdersQueryError(t *testing.T) {
	calls := fakeBackends(t, func(string, int) int { return http.StatusOK })
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("WHERE status = 'pending'")).WillReturnError(errors.New("db down"))

	detectAbandonedOrders()

	if got := calls(); len(got) != 0 {
		t.Errorf("calls = %+v, want none", got)
	}
}

func TestCallService(t *testing.T) {
	headers := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	tests := []struct {
		name     string
		url      string
		wantCode int
		wantErr  bool
	}{
		{"returns the status code", srv.URL + "/status", http.StatusAccepted, false},
		{"bad url", "://nope", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, err := callService(http.MethodPut, tt.url, map[string]int{"order_id": 1})
			if (err != nil) != tt.wantErr || code != tt.wantCode {
				t.Fatalf("callService = %d, %v; want %d, err %v", code, err, tt.wantCode, tt.wantErr)
			}
		})
	}
	// The backends require X-User-Email (#126), so the scheduler names itself
	got := <-headers
	if got.Get("X-User-Email") != "scheduler" || got.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", got)
	}
}

// Pins #19: runEvery runs the job straight away, starts no new run after the
// context is cancelled, and does not return while a run is in progress.
func TestRunEveryFinishesRunningJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	job := func() {
		if runs.Add(1) == 1 {
			close(started)
			<-release
		}
	}

	done := make(chan struct{})
	go func() {
		runEvery(ctx, time.Millisecond, "test", job)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("job did not run on start")
	}

	cancel()
	select {
	case <-done:
		t.Fatal("runEvery returned while the job was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runEvery did not return after the job finished")
	}
	if n := runs.Load(); n != 1 {
		t.Errorf("job ran %d times, want 1: no run may start after cancel", n)
	}
}

func TestRunEveryRepeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int32
	done := make(chan struct{})
	go func() {
		runEvery(ctx, 5*time.Millisecond, "test", func() { runs.Add(1) })
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for runs.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("ran %d times in 2s, want at least 3", runs.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	after := runs.Load()
	time.Sleep(20 * time.Millisecond)
	if runs.Load() != after {
		t.Errorf("job ran after runEvery returned")
	}
}

func TestExpireReservations(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("WHERE r.status = 'active' AND r.expires_at < NOW()")).WillReturnRows(
		sqlmock.NewRows([]string{"order_id", "product_id", "quantity"}).AddRow(5, "p1", 2).AddRow(6, "p2", 1))
	for _, r := range []struct {
		order   int
		product string
		qty     int
	}{{5, "p1", 2}, {6, "p2", 1}} {
		mock.ExpectBegin()
		mock.ExpectExec(sqlText("UPDATE products SET reserved = GREATEST(reserved - $1, 0)")).WithArgs(r.qty, r.product).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(sqlText("UPDATE reservations SET status = 'expired'")).WithArgs(r.order, r.product).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
	}

	expireReservations()
}

func TestCleanupOldEvents(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"deletes old events", nil},
		{"delete fails", errors.New("db down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			e := mock.ExpectExec(sqlText("DELETE FROM order_events WHERE created_at < $1")).WithArgs(sqlmock.AnyArg())
			if tt.err != nil {
				e.WillReturnError(tt.err)
			} else {
				e.WillReturnResult(sqlmock.NewResult(0, 12))
			}
			cleanupOldEvents()
		})
	}
}

func TestGenerateDigestAndRetryQueries(t *testing.T) {
	mock := mockDB(t)
	one := func(v interface{}) *sqlmock.Rows { return sqlmock.NewRows([]string{"v"}).AddRow(v) }
	mock.ExpectQuery(sqlText("WHERE status = 'failed' AND created_at > $1")).WithArgs(sqlmock.AnyArg()).WillReturnRows(one(2))
	mock.ExpectQuery(sqlText("FROM orders WHERE created_at >= $1")).WillReturnRows(one(10))
	mock.ExpectQuery(sqlText("FROM orders WHERE status = 'pending'")).WillReturnRows(one(3))
	mock.ExpectQuery(sqlText("WHERE status = 'delivered' AND updated_at >= $1")).WillReturnRows(one(4))
	mock.ExpectQuery(sqlText("FROM payments WHERE status = 'completed'")).WillReturnRows(one(99.5))

	retryFailedPayments()
	generateDigest()
}
