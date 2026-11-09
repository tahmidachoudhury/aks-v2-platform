package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
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

			detectAbandonedOrders(context.Background())

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
	detectAbandonedOrders(context.Background())
}

// Pins #19: a shutdown mid-run aborts the call in flight and starts no more,
// so the job cannot hold up the scheduler's exit.
func TestDetectAbandonedOrdersStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hang := make(chan struct{})
	calls := fakeBackends(t, func(_ string, id int) int {
		// Shut down while order-service is handling the first cancel, and
		// never answer it
		if id == 5 {
			cancel()
			<-hang
		}
		return http.StatusOK
	})
	t.Cleanup(func() { close(hang) })
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("WHERE status = 'pending'")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(5).AddRow(6).AddRow(7))
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	done := make(chan struct{})
	go func() {
		detectAbandonedOrders(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("detectAbandonedOrders did not return after its context was cancelled")
	}

	got := calls()
	if len(got) != 1 || got[0].Path != "/status" || got[0].Body["order_id"] != float64(5) {
		t.Errorf("calls = %+v, want only the cancel for order 5", got)
	}
	// Orders 6 and 7 are left for the next run, not tried and failed
	if !strings.Contains(logs.String(), "2 orders left for the next run") || strings.Contains(logs.String(), "order #6") {
		t.Errorf("logs = %q, want the loop to stop before order 6", logs.String())
	}
}

func TestDetectAbandonedOrdersCancelledBeforeStart(t *testing.T) {
	calls := fakeBackends(t, func(string, int) int { return http.StatusOK })
	mockDB(t) // a cancelled context runs no query

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	detectAbandonedOrders(ctx)

	if got := calls(); len(got) != 0 {
		t.Errorf("calls = %+v, want none", got)
	}
}

func TestDetectAbandonedOrdersQueryError(t *testing.T) {
	calls := fakeBackends(t, func(string, int) int { return http.StatusOK })
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("WHERE status = 'pending'")).WillReturnError(errors.New("db down"))

	detectAbandonedOrders(context.Background())

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
			code, err := callService(context.Background(), http.MethodPut, tt.url, map[string]int{"order_id": 1})
			if (err != nil) != tt.wantErr || code != tt.wantCode {
				t.Fatalf("callService = %d, %v; want %d, err %v", code, err, tt.wantCode, tt.wantErr)
			}
		})
	}
	// The backends require X-User-Email (#126), so the scheduler names itself,
	// and role service lets it cancel any customer's order
	got := <-headers
	if got.Get("X-User-Email") != "scheduler" || got.Get("X-User-Role") != "service" || got.Get("Content-Type") != "application/json" {
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
	var jobCtx context.Context
	job := func(ctx context.Context) {
		if runs.Add(1) == 1 {
			jobCtx = ctx
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

	// The shutdown log names the jobs it would abandon
	if got := runningJobs(); len(got) != 1 || got[0] != "test" {
		t.Errorf("runningJobs = %v while the job runs, want [test]", got)
	}

	cancel()
	select {
	case <-done:
		t.Fatal("runEvery returned while the job was still running")
	case <-time.After(50 * time.Millisecond):
	}
	if jobCtx.Err() == nil {
		t.Error("the job's context was not cancelled with runEvery's")
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
	if got := runningJobs(); len(got) != 0 {
		t.Errorf("runningJobs = %v after the job finished, want none", got)
	}
}

// Pins #19: after shutdown starts, no job queries the database, so none is
// left running when main closes it.
func TestJobsStopWhenCancelled(t *testing.T) {
	jobs := map[string]func(context.Context){
		"expire_reservations":   expireReservations,
		"abandoned_carts":       detectAbandonedOrders,
		"retry_failed_payments": retryFailedPayments,
		"daily_digest":          generateDigest,
		"cleanup_old_events":    cleanupOldEvents,
	}
	for name, job := range jobs {
		t.Run(name, func(t *testing.T) {
			mockDB(t) // any SQL fails the test
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			job(ctx)
		})
	}
}

// expire_reservations stops between reservations once shutdown starts, and
// leaves the rest active for the next run.
func TestExpireReservationsStopsMidLoop(t *testing.T) {
	mock := mockDB(t)
	mock.ExpectQuery(sqlText("SELECT id FROM reservations")).WillReturnRows(
		sqlmock.NewRows([]string{"id"}).AddRow(1).AddRow(2).AddRow(3))
	mock.ExpectBegin()
	mock.ExpectQuery(sqlText("UPDATE reservations SET status = 'expired'")).WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"product_id", "quantity"}).AddRow("p1", 2))
	// Shutdown starts while the first stock update runs
	mock.ExpectExec(sqlText("UPDATE products")).WithArgs(2, "p1").
		WillDelayFor(100 * time.Millisecond).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	expireReservations(ctx)

	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Errorf("took %s, want it to stop when cancelled", waited)
	}
	if !strings.Contains(logs.String(), "2 reservations left for the next run") {
		t.Errorf("logs = %q, want the loop to stop before reservation 2", logs.String())
	}
	// The rolled-back transaction may finish just after the job returns
	for deadline := time.Now().Add(time.Second); mock.ExpectationsWereMet() != nil && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
}

// Pins #19: the scheduler waits for running jobs, but not past the bound.
func TestWaitTimeout(t *testing.T) {
	tests := []struct {
		name    string
		jobTime time.Duration
		want    bool
	}{
		{"job finishes in time", 0, true},
		{"job outlasts the bound", time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(tt.jobTime)
			}()
			start := time.Now()
			if got := waitTimeout(&wg, 100*time.Millisecond); got != tt.want {
				t.Errorf("waitTimeout = %v, want %v", got, tt.want)
			}
			if waited := time.Since(start); waited > 500*time.Millisecond {
				t.Errorf("waited %s, want at most the 100ms bound", waited)
			}
			wg.Wait()
		})
	}
}

func TestRunEveryRepeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int32
	done := make(chan struct{})
	go func() {
		runEvery(ctx, 5*time.Millisecond, "test", func(context.Context) { runs.Add(1) })
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

// Each expired reservation is claimed with a conditional UPDATE before its
// stock is returned, so overlapping runs return it once. Errors roll back
// that reservation and the run moves on.
func TestExpireReservations(t *testing.T) {
	claim := sqlText("UPDATE reservations SET status = 'expired'")
	release := sqlText("UPDATE products SET reserved = GREATEST(reserved - $1, 0)")
	claimed := func(product string, qty int) *sqlmock.Rows {
		return sqlmock.NewRows([]string{"product_id", "quantity"}).AddRow(product, qty)
	}
	tests := []struct {
		name  string
		setup func(mock sqlmock.Sqlmock)
		want  string // expected log line
	}{
		{
			name: "returns the stock of each reservation",
			setup: func(mock sqlmock.Sqlmock) {
				for _, r := range []struct {
					id      int
					product string
					qty     int
				}{{1, "p1", 2}, {2, "p2", 1}} {
					mock.ExpectBegin()
					mock.ExpectQuery(claim).WithArgs(r.id).WillReturnRows(claimed(r.product, r.qty))
					mock.ExpectExec(release).WithArgs(r.qty, r.product).WillReturnResult(sqlmock.NewResult(0, 1))
					mock.ExpectCommit()
				}
			},
			want: "Expired 2 reservations",
		},
		{
			name: "already expired by another run is skipped",
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(claim).WithArgs(1).WillReturnRows(sqlmock.NewRows([]string{"product_id", "quantity"}))
				mock.ExpectRollback()
				mock.ExpectBegin()
				mock.ExpectQuery(claim).WithArgs(2).WillReturnRows(claimed("p2", 1))
				mock.ExpectExec(release).WithArgs(1, "p2").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			},
			want: "Expired 1 reservations",
		},
		{
			name: "failed stock update rolls back and moves on",
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(claim).WithArgs(1).WillReturnRows(claimed("p1", 2))
				mock.ExpectExec(release).WithArgs(2, "p1").WillReturnError(errors.New("db down"))
				mock.ExpectRollback()
				mock.ExpectBegin()
				mock.ExpectQuery(claim).WithArgs(2).WillReturnRows(claimed("p2", 1))
				mock.ExpectExec(release).WithArgs(1, "p2").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			},
			want: "reservation 1: db down",
		},
		{
			name: "failed claim and failed commit are logged",
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(claim).WithArgs(1).WillReturnError(errors.New("lock timeout"))
				mock.ExpectRollback()
				mock.ExpectBegin()
				mock.ExpectQuery(claim).WithArgs(2).WillReturnRows(claimed("p2", 1))
				mock.ExpectExec(release).WithArgs(1, "p2").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit().WillReturnError(errors.New("serialization failure"))
			},
			want: "reservation 2: serialization failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			mock.ExpectQuery(sqlText("SELECT id FROM reservations")).WillReturnRows(
				sqlmock.NewRows([]string{"id"}).AddRow(1).AddRow(2))
			tt.setup(mock)
			var logs bytes.Buffer
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(io.Discard) })

			expireReservations(context.Background())

			if !strings.Contains(logs.String(), tt.want) {
				t.Errorf("logs = %q, want %q", logs.String(), tt.want)
			}
		})
	}
}

func TestExpireReservationsQueryErrors(t *testing.T) {
	tests := []struct {
		name string
		rows func(mock sqlmock.Sqlmock)
	}{
		{"query fails", func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("SELECT id FROM reservations")).WillReturnError(errors.New("db down"))
		}},
		{"scan fails", func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("SELECT id FROM reservations")).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("not-a-number"))
		}},
		{"row error", func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(sqlText("SELECT id FROM reservations")).WillReturnRows(
				sqlmock.NewRows([]string{"id"}).AddRow(1).RowError(0, errors.New("connection reset")))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := mockDB(t)
			tt.rows(mock)
			// Nothing else may run: sqlmock fails any statement not expected
			expireReservations(context.Background())
		})
	}
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
			cleanupOldEvents(context.Background())
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

	retryFailedPayments(context.Background())
	generateDigest(context.Background())
}
