package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

//go:embed static
var staticFiles embed.FS

var db *sql.DB

// Set on SIGTERM so /healthz fails and the pod leaves the Service endpoints
var shuttingDown atomic.Bool

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

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(3)
	db.SetConnMaxLifetime(5 * time.Minute)
	waitForDB()

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/dashboard/healthz", handleHealth)
	mux.HandleFunc("/dashboard/summary", handleSummary)
	mux.HandleFunc("/dashboard/orders/stats", handleOrderStats)
	mux.HandleFunc("/dashboard/revenue", handleRevenue)
	mux.HandleFunc("/dashboard/inventory/alerts", handleInventoryAlerts)
	mux.HandleFunc("/dashboard/shipping/overview", handleShippingOverview)

	// Serve frontend UI
	staticFS, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	port := getEnv("PORT", "8086")
	drainDelay, err := time.ParseDuration(getEnv("SHUTDOWN_DRAIN_DELAY", "5s"))
	if err != nil {
		log.Fatalf("invalid SHUTDOWN_DRAIN_DELAY: %v", err)
	}
	server := &http.Server{
		Addr:         ":" + port,
		Handler:      requireAdmin(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("Dashboard API listening on :%s", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	// Fail readiness and keep serving while the pod leaves the Service endpoints,
	// then stop accepting connections and let in-flight requests finish
	log.Printf("Shutting down, draining for %s", drainDelay)
	shuttingDown.Store(true)
	time.Sleep(drainDelay)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown error: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	if shuttingDown.Load() {
		status = "shutting down"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else if err := db.Ping(); err != nil {
		status = "unhealthy"
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": status, "service": "dashboard-api"})
}

// queries runs a handler's queries and keeps the first error, so the handler
// can check once and return 500 rather than zeros. After an error, later
// calls do nothing.
type queries struct{ err error }

// value scans a one-row, one-column query into dest.
func (q *queries) value(dest interface{}, query string, args ...interface{}) {
	if q.err == nil {
		q.err = db.QueryRow(query, args...).Scan(dest)
	}
}

// each runs query and calls scan for every row.
func (q *queries) each(query string, scan func(*sql.Rows) error) {
	if q.err != nil {
		return
	}
	rows, err := db.Query(query)
	if err != nil {
		q.err = err
		return
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			q.err = err
			return
		}
	}
	q.err = rows.Err()
}

// counts reads a "key, COUNT(*)" query into a map.
func (q *queries) counts(query string) map[string]int {
	m := map[string]int{}
	q.each(query, func(rows *sql.Rows) error {
		var key string
		var n int
		err := rows.Scan(&key, &n)
		m[key] = n
		return err
	})
	return m
}

// failed writes a 500 and reports true if any query failed.
func (q *queries) failed(w http.ResponseWriter, handler string) bool {
	if q.err == nil {
		return false
	}
	log.Printf("%s query error: %v", handler, q.err)
	httpError(w, "query failed", http.StatusInternalServerError)
	return true
}

func handleSummary(w http.ResponseWriter, r *http.Request) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var q queries

	// Order counts
	var totalOrders, ordersToday int
	q.value(&totalOrders, "SELECT COUNT(*) FROM orders")
	q.value(&ordersToday, "SELECT COUNT(*) FROM orders WHERE created_at >= $1", today)

	// Orders by status
	statusCounts := q.counts("SELECT status, COUNT(*) FROM orders GROUP BY status")

	// Revenue (charges minus refunds)
	var totalCharges, totalRefunds, todayCharges, todayRefunds float64
	q.value(&totalCharges, "SELECT COALESCE(SUM(amount), 0) FROM payments WHERE status IN ('completed','partially_refunded','refunded') AND (method IS NULL OR method != 'refund')")
	q.value(&totalRefunds, "SELECT COALESCE(SUM(amount), 0) FROM payments WHERE status = 'completed' AND method = 'refund'")
	q.value(&todayCharges, "SELECT COALESCE(SUM(amount), 0) FROM payments WHERE status IN ('completed','partially_refunded','refunded') AND (method IS NULL OR method != 'refund') AND created_at >= $1", today)
	q.value(&todayRefunds, "SELECT COALESCE(SUM(amount), 0) FROM payments WHERE status = 'completed' AND method = 'refund' AND created_at >= $1", today)

	// Product count
	var totalProducts int
	q.value(&totalProducts, "SELECT COUNT(*) FROM products")

	// Low stock count
	var lowStockCount int
	q.value(&lowStockCount, "SELECT COUNT(*) FROM products WHERE (stock - reserved) < 10")

	// Active shipments
	var activeShipments int
	q.value(&activeShipments, "SELECT COUNT(*) FROM shipments WHERE status NOT IN ('delivered', 'cancelled')")

	if q.failed(w, "summary") {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"orders": map[string]interface{}{
			"total":     totalOrders,
			"today":     ordersToday,
			"by_status": statusCounts,
		},
		"revenue": map[string]interface{}{
			"total":    totalCharges - totalRefunds,
			"today":    todayCharges - todayRefunds,
			"currency": "GBP",
		},
		"inventory": map[string]interface{}{
			"total_products": totalProducts,
			"low_stock":      lowStockCount,
		},
		"shipping": map[string]interface{}{
			"active_shipments": activeShipments,
		},
	})
}

func handleOrderStats(w http.ResponseWriter, r *http.Request) {
	type DayStat struct {
		Date    string  `json:"date"`
		Orders  int     `json:"orders"`
		Revenue float64 `json:"revenue"`
	}

	// Orders per day for last 30 days
	var q queries
	stats := []DayStat{}
	q.each(
		`SELECT DATE(created_at) as day, COUNT(*), COALESCE(SUM(total), 0)
		 FROM orders
		 WHERE created_at >= NOW() - INTERVAL '30 days'
		 GROUP BY DATE(created_at)
		 ORDER BY day DESC`,
		func(rows *sql.Rows) error {
			var s DayStat
			err := rows.Scan(&s.Date, &s.Orders, &s.Revenue)
			stats = append(stats, s)
			return err
		},
	)
	if q.failed(w, "order stats") {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func handleRevenue(w http.ResponseWriter, r *http.Request) {
	type DayRevenue struct {
		Date    string  `json:"date"`
		Revenue float64 `json:"revenue"`
	}

	// Revenue breakdown
	var q queries
	var total, refunded float64
	q.value(&total, "SELECT COALESCE(SUM(amount), 0) FROM payments WHERE status IN ('completed', 'partially_refunded', 'refunded') AND (method IS NULL OR method != 'refund')")
	q.value(&refunded, "SELECT COALESCE(SUM(amount), 0) FROM payments WHERE status = 'completed' AND method = 'refund'")

	// Revenue by day (last 7 days)
	daily := []DayRevenue{}
	q.each(
		`SELECT DATE(created_at) as day, COALESCE(SUM(amount), 0)
		 FROM payments
		 WHERE status IN ('completed', 'partially_refunded', 'refunded') AND (method IS NULL OR method != 'refund')
		 AND created_at >= NOW() - INTERVAL '7 days'
		 GROUP BY DATE(created_at)
		 ORDER BY day DESC`,
		func(rows *sql.Rows) error {
			var d DayRevenue
			err := rows.Scan(&d.Date, &d.Revenue)
			daily = append(daily, d)
			return err
		},
	)
	if q.failed(w, "revenue") {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":    total,
		"refunded": refunded,
		"net":      total - refunded,
		"currency": "GBP",
		"daily":    daily,
	})
}

func handleInventoryAlerts(w http.ResponseWriter, r *http.Request) {
	type Alert struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		SKU       string `json:"sku"`
		Stock     int    `json:"stock"`
		Reserved  int    `json:"reserved"`
		Available int    `json:"available"`
	}

	var q queries
	alerts := []Alert{}
	q.each(
		`SELECT id, name, sku, stock, reserved, (stock - reserved) as available
		 FROM products
		 WHERE (stock - reserved) < 10
		 ORDER BY (stock - reserved) ASC`,
		func(rows *sql.Rows) error {
			var a Alert
			err := rows.Scan(&a.ID, &a.Name, &a.SKU, &a.Stock, &a.Reserved, &a.Available)
			alerts = append(alerts, a)
			return err
		},
	)
	if q.failed(w, "inventory alerts") {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(alerts)
}

func handleShippingOverview(w http.ResponseWriter, r *http.Request) {
	var q queries
	statusCounts := q.counts("SELECT status, COUNT(*) FROM shipments GROUP BY status")
	carrierCounts := q.counts("SELECT carrier, COUNT(*) FROM shipments GROUP BY carrier")

	// Average delivery time
	var avgDeliveryHours float64
	q.value(&avgDeliveryHours,
		`SELECT COALESCE(AVG(EXTRACT(EPOCH FROM (delivered_at - shipped_at)) / 3600), 0)
		 FROM shipments WHERE delivered_at IS NOT NULL AND shipped_at IS NOT NULL`,
	)
	if q.failed(w, "shipping overview") {
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"by_status":          statusCounts,
		"by_carrier":         carrierCounts,
		"avg_delivery_hours": avgDeliveryHours,
	})
}

// requireAdmin limits the /dashboard/* data to admins. It returns 401
// without the X-User-Email header that api-gateway sets from the JWT, and 403
// unless X-User-Role is admin. The UI page and health checks are exempt.
func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dashboard/") && r.URL.Path != "/dashboard/healthz" {
			if r.Header.Get("X-User-Email") == "" {
				httpError(w, "missing user identity", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("X-User-Role") != "admin" {
				httpError(w, "admin only", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func httpError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
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
