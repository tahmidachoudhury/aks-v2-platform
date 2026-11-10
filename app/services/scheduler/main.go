package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

var (
	// running holds the names of jobs that are mid-run, for the shutdown log
	running sync.Map

	db         *sql.DB
	httpClient = &http.Client{Timeout: 10 * time.Second}
	services   map[string]string
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	var err error
	db, err = sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Internal service URLs. Abandoned orders are cancelled through order-service
	// and their stock released through inventory-service.
	services = map[string]string{
		"order":     getEnv("ORDER_SERVICE_URL", "http://order-service:8081"),
		"inventory": getEnv("INVENTORY_SERVICE_URL", "http://inventory-service:8082"),
	}

	shutdownTimeout, err := time.ParseDuration(getEnv("SHUTDOWN_TIMEOUT", "20s"))
	if err != nil {
		log.Fatalf("invalid SHUTDOWN_TIMEOUT: %v", err)
	}

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	waitForDB()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Health check
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		status := "ok"
		w.Header().Set("Content-Type", "application/json")
		if err := db.Ping(); err != nil {
			status = "unhealthy"
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(map[string]string{"status": status, "service": "scheduler"})
	})
	health := &http.Server{Addr: ":" + getEnv("HEALTH_PORT", "8091"), Handler: mux}
	go func() {
		log.Printf("Scheduler health check on %s", health.Addr)
		if err := health.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("health server error: %v", err)
		}
	}()

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Shutting down scheduler...")
		cancel()
	}()

	log.Println("Scheduler started")

	// Run scheduled jobs. Each takes ctx and stops between steps once it is
	// cancelled. On shutdown no new run starts, and the scheduler waits up to
	// shutdownTimeout for a running job before it closes the database.
	var jobs sync.WaitGroup
	schedule := func(interval time.Duration, name string, fn func(context.Context)) {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			runEvery(ctx, interval, name, fn)
		}()
	}
	schedule(1*time.Minute, "expire_reservations", expireReservations)
	schedule(5*time.Minute, "abandoned_carts", detectAbandonedOrders)
	schedule(15*time.Minute, "retry_failed_payments", retryFailedPayments)
	schedule(1*time.Hour, "daily_digest", generateDigest)
	schedule(30*time.Minute, "cleanup_old_events", cleanupOldEvents)

	<-ctx.Done()
	log.Printf("Waiting up to %s for running jobs to finish", shutdownTimeout)
	if !waitTimeout(&jobs, shutdownTimeout) {
		log.Printf("Jobs still running after %s, abandoning %s: closing the database under them", shutdownTimeout, strings.Join(runningJobs(), ", "))
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	health.Shutdown(shutdownCtx)
	log.Println("Scheduler stopped")
}

// waitTimeout waits for wg, giving up after d. It reports whether wg finished.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// runningJobs lists the jobs that are mid-run, sorted.
func runningJobs() []string {
	var names []string
	running.Range(func(name, _ interface{}) bool {
		names = append(names, name.(string))
		return true
	})
	sort.Strings(names)
	return names
}

func runEvery(ctx context.Context, interval time.Duration, name string, fn func(context.Context)) {
	log.Printf("Scheduled job '%s' every %s", name, interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	run := func() {
		running.Store(name, true)
		defer running.Delete(name)
		fn(ctx)
	}

	// Run immediately on start
	log.Printf("Running job: %s", name)
	run()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			log.Printf("Running job: %s", name)
			start := time.Now()
			run()
			log.Printf("Job '%s' completed in %s", name, time.Since(start))
		}
	}
}

// expireReservations releases inventory reservations that have expired
func expireReservations(ctx context.Context) {
	rows, err := db.QueryContext(ctx,
		`SELECT id FROM reservations
		 WHERE status = 'active' AND expires_at < NOW()`,
	)
	if err != nil {
		log.Printf("expire_reservations query error: %v", err)
		return
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			log.Printf("expire_reservations scan error: %v", err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("expire_reservations query error: %v", err)
		return
	}

	count := 0
	for i, id := range ids {
		if ctx.Err() != nil {
			log.Printf("expire_reservations: shutting down, %d reservations left for the next run", len(ids)-i)
			break
		}
		released, err := expireReservation(ctx, id)
		if err != nil {
			log.Printf("expire_reservations: reservation %d: %v", id, err)
			continue
		}
		if released {
			count++
		}
	}

	if count > 0 {
		log.Printf("Expired %d reservations", count)
	}
}

// expireReservation marks one reservation expired and returns its stock, in
// one transaction. The UPDATE claims the reservation only while it is still
// active, so when two runs overlap, or inventory-service releases it first,
// the stock is returned once. It reports whether this call returned it.
// A cancelled ctx rolls the transaction back, leaving the reservation active.
func expireReservation(ctx context.Context, id int) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var productID string
	var quantity int
	err = tx.QueryRow(
		`UPDATE reservations SET status = 'expired'
		 WHERE id = $1 AND status = 'active'
		 RETURNING product_id, quantity`,
		id,
	).Scan(&productID, &quantity)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if _, err := tx.Exec(
		"UPDATE products SET reserved = GREATEST(reserved - $1, 0), updated_at = NOW() WHERE id = $2",
		quantity, productID,
	); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// detectAbandonedOrders finds orders stuck in pending for too long. It stops
// between orders, and aborts its service calls, once ctx is cancelled.
func detectAbandonedOrders(ctx context.Context) {
	cutoff := time.Now().Add(-30 * time.Minute)

	rows, err := db.QueryContext(ctx,
		`SELECT id FROM orders
		 WHERE status = 'pending' AND created_at < $1`,
		cutoff,
	)
	if err != nil {
		log.Printf("abandoned_carts query error: %v", err)
		return
	}
	var orders []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			log.Printf("abandoned_carts scan error: %v", err)
			return
		}
		orders = append(orders, id)
	}
	rows.Close()

	count := 0
	for i, id := range orders {
		if ctx.Err() != nil {
			log.Printf("abandoned_carts: shutting down, %d orders left for the next run", len(orders)-i)
			break
		}
		// Cancel through order-service's state machine, which writes the
		// order_events row and publishes order.status_changed
		code, err := callService(ctx, http.MethodPut, services["order"]+"/status",
			map[string]interface{}{"order_id": id, "new_status": "cancelled"})
		if err != nil || code != http.StatusOK {
			log.Printf("abandoned_carts: cancelling order #%d failed (status %d): %v", id, code, err)
			continue
		}

		// Release the reserved stock. If this fails, expire_reservations
		// releases it once the reservation expires.
		code, err = callService(ctx, http.MethodPost, services["inventory"]+"/release",
			map[string]interface{}{"order_id": id})
		if err != nil || code != http.StatusOK {
			log.Printf("abandoned_carts: releasing stock for order #%d failed (status %d): %v", id, code, err)
		}

		log.Printf("Cancelled abandoned order #%d", id)
		count++
	}

	if count > 0 {
		log.Printf("Cancelled %d abandoned orders", count)
	}
}

// retryFailedPayments attempts to re-process failed payments
func retryFailedPayments(ctx context.Context) {
	cutoff := time.Now().Add(-1 * time.Hour)

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM payments
		 WHERE status = 'failed' AND created_at > $1`,
		cutoff,
	).Scan(&count); err != nil {
		log.Printf("retry_failed_payments query error: %v", err)
		return
	}

	if count > 0 {
		log.Printf("Found %d failed payments eligible for retry", count)
		// In production: re-queue payment attempts via SQS
		// POST to payment-service/charge for each
	}
}

// generateDigest creates summary stats
func generateDigest(ctx context.Context) {
	var totalOrders, pendingOrders, completedOrders int
	var totalRevenue float64

	today := time.Now().UTC().Truncate(24 * time.Hour)

	// Each query runs with ctx, so a shutdown stops the digest between them
	for _, q := range []struct {
		dest  interface{}
		query string
		args  []interface{}
	}{
		{&totalOrders, "SELECT COUNT(*) FROM orders WHERE created_at >= $1", []interface{}{today}},
		{&pendingOrders, "SELECT COUNT(*) FROM orders WHERE status = 'pending'", nil},
		{&completedOrders, "SELECT COUNT(*) FROM orders WHERE status = 'delivered' AND updated_at >= $1", []interface{}{today}},
		{&totalRevenue, "SELECT COALESCE(SUM(amount), 0) FROM payments WHERE status = 'completed' AND created_at >= $1", []interface{}{today}},
	} {
		if err := db.QueryRowContext(ctx, q.query, q.args...).Scan(q.dest); err != nil {
			log.Printf("daily_digest query error: %v", err)
			return
		}
	}

	log.Printf("Daily digest - Orders today: %d, Pending: %d, Completed: %d, Revenue: %.2f",
		totalOrders, pendingOrders, completedOrders, totalRevenue)

	// In production: send digest via notification-service
}

// cleanupOldEvents removes old tracking/audit data
func cleanupOldEvents(ctx context.Context) {
	cutoff := time.Now().AddDate(0, 0, -90) // 90 days retention

	result, err := db.ExecContext(ctx, "DELETE FROM order_events WHERE created_at < $1", cutoff)
	if err != nil {
		log.Printf("cleanup error: %v", err)
		return
	}

	if rows, _ := result.RowsAffected(); rows > 0 {
		log.Printf("Cleaned up %d old order events", rows)
	}
}

// callService sends a JSON request to another service and returns the status
// code. The backends require X-User-Email, so the scheduler names itself, and
// role service lets it act on any customer's order.
func callService(ctx context.Context, method, url string, body interface{}) (int, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Email", "scheduler")
	req.Header.Set("X-User-Role", "service")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func waitForDB() {
	for i := 0; i < 120; i++ {
		if err := db.Ping(); err == nil {
			return
		}
		log.Printf("Waiting for database... (%d/120)", i+1)
		time.Sleep(time.Second)
	}
	log.Fatal("Database not ready after 120s")
}
