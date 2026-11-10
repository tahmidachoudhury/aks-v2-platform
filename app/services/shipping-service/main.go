package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	_ "github.com/lib/pq"
)

var db *sql.DB

// Set on SIGTERM so /healthz fails and the pod leaves the Service endpoints
var shuttingDown atomic.Bool

var (
	sbClient *azservicebus.Client
	sbSender *azservicebus.Sender
	sbQueue  string
)

// Shared secret carriers send in X-Webhook-Secret, from SHIPPING_WEBHOOK_SECRET
var webhookSecret string

// Statuses the carrier webhook accepts. The status becomes part of the event name.
var webhookStatuses = map[string]bool{
	"label_created": true,
	"in_transit":    true,
	"delivered":     true,
	"cancelled":     true,
}

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

	webhookSecret = os.Getenv("SHIPPING_WEBHOOK_SECRET")
	if webhookSecret == "" {
		log.Println("WARNING: SHIPPING_WEBHOOK_SECRET not set, the carrier webhook will reject every request")
	}

	db.SetMaxOpenConns(15)
	db.SetMaxIdleConns(3)
	db.SetConnMaxLifetime(5 * time.Minute)
	waitForDB()
	migrate()
	connectServiceBus()

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/shipments", handleShipments)
	mux.HandleFunc("/shipments/", handleShipment)
	mux.HandleFunc("/track/", handleTrack)
	mux.HandleFunc("/webhook", handleCarrierWebhook)

	port := getEnv("PORT", "8085")
	drainDelay, err := time.ParseDuration(getEnv("SHUTDOWN_DRAIN_DELAY", "5s"))
	if err != nil {
		log.Fatalf("invalid SHUTDOWN_DRAIN_DELAY: %v", err)
	}
	server := &http.Server{
		Addr:         ":" + port,
		Handler:      requireUser(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("Shipping service listening on :%s", port)
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
	if sbClient != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		sbSender.Close(closeCtx)
		sbClient.Close(closeCtx)
	}
}

func migrate() {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS shipments (
			id SERIAL PRIMARY KEY,
			order_id INTEGER NOT NULL,
			carrier VARCHAR(50) NOT NULL,
			tracking_number VARCHAR(100) UNIQUE,
			status VARCHAR(30) NOT NULL DEFAULT 'label_created',
			recipient_name VARCHAR(255),
			address_line1 TEXT,
			address_line2 TEXT,
			city VARCHAR(100),
			postcode VARCHAR(20),
			country VARCHAR(2) DEFAULT 'GB',
			weight_kg DECIMAL(8,2),
			estimated_delivery DATE,
			shipped_at TIMESTAMP,
			delivered_at TIMESTAMP,
			created_at TIMESTAMP DEFAULT NOW(),
			updated_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_shipments_order ON shipments(order_id)`,
		`CREATE INDEX IF NOT EXISTS idx_shipments_tracking ON shipments(tracking_number)`,
		`CREATE TABLE IF NOT EXISTS tracking_events (
			id SERIAL PRIMARY KEY,
			shipment_id INTEGER NOT NULL REFERENCES shipments(id),
			status VARCHAR(30) NOT NULL,
			location VARCHAR(255),
			description TEXT,
			occurred_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tracking_shipment ON tracking_events(shipment_id)`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			log.Fatalf("Migration failed: %v", err)
		}
	}
	log.Println("Shipping service migrations complete")
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	if shuttingDown.Load() {
		status = "shutting down"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else if err := db.Ping(); err != nil {
		status = "unhealthy"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else if err := checkServiceBus(r.Context()); err != nil {
		status = "unhealthy"
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": status, "service": "shipping-service"})
}

func handleShipments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		query := `SELECT s.id, s.order_id, s.carrier, s.tracking_number, s.status, s.recipient_name,
			        s.city, s.country, s.estimated_delivery, s.created_at
			 FROM shipments s`
		args := []interface{}{}
		// Customers see shipments for their own orders. Admin and service
		// callers see all.
		if !privileged(r) {
			query += ` JOIN orders o ON o.id = s.order_id WHERE o.customer_id = $1`
			args = append(args, r.Header.Get("X-User-Email"))
		}
		query += ` ORDER BY s.created_at DESC LIMIT 100`

		rows, err := db.Query(query, args...)
		if err != nil {
			httpError(w, "query failed", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type ShipmentSummary struct {
			ID                int    `json:"id"`
			OrderID           int    `json:"order_id"`
			Carrier           string `json:"carrier"`
			TrackingNumber    string `json:"tracking_number"`
			Status            string `json:"status"`
			RecipientName     string `json:"recipient_name"`
			City              string `json:"city"`
			Country           string `json:"country"`
			EstimatedDelivery string `json:"estimated_delivery,omitempty"`
			CreatedAt         string `json:"created_at"`
		}

		// As in handleShipment, the nullable columns scan into Null types
		shipments := []ShipmentSummary{}
		for rows.Next() {
			var s ShipmentSummary
			var tracking, recipient, city, country, estDel sql.NullString
			if err := rows.Scan(&s.ID, &s.OrderID, &s.Carrier, &tracking, &s.Status, &recipient,
				&city, &country, &estDel, &s.CreatedAt); err != nil {
				log.Printf("shipments scan error: %v", err)
				httpError(w, "scan failed", http.StatusInternalServerError)
				return
			}
			s.TrackingNumber, s.RecipientName, s.City, s.Country = tracking.String, recipient.String, city.String, country.String
			s.EstimatedDelivery = estDel.String
			shipments = append(shipments, s)
		}
		if err := rows.Err(); err != nil {
			log.Printf("shipments query error: %v", err)
			httpError(w, "query failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(shipments)

	case http.MethodPost:
		// Shipments are created by internal callers and admins, not customers
		if !privileged(r) {
			httpError(w, "forbidden", http.StatusForbidden)
			return
		}
		createShipment(w, r)

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func createShipment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderID       int     `json:"order_id"`
		Carrier       string  `json:"carrier"`
		RecipientName string  `json:"recipient_name"`
		AddressLine1  string  `json:"address_line1"`
		AddressLine2  string  `json:"address_line2"`
		City          string  `json:"city"`
		Postcode      string  `json:"postcode"`
		Country       string  `json:"country"`
		WeightKg      float64 `json:"weight_kg"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.OrderID == 0 || req.RecipientName == "" {
		httpError(w, "order_id and recipient_name required", http.StatusBadRequest)
		return
	}

	carrier := req.Carrier
	if carrier == "" {
		carrier = "royal_mail"
	}
	country := req.Country
	if country == "" {
		country = "GB"
	}

	trackingNumber := generateTrackingNumber(carrier)
	estimatedDelivery := time.Now().AddDate(0, 0, 3+rand.Intn(5))

	var shipmentID int
	err := db.QueryRow(
		`INSERT INTO shipments (order_id, carrier, tracking_number, recipient_name,
		 address_line1, address_line2, city, postcode, country, weight_kg, estimated_delivery)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
		req.OrderID, carrier, trackingNumber, req.RecipientName,
		req.AddressLine1, req.AddressLine2, req.City, req.Postcode, country,
		req.WeightKg, estimatedDelivery,
	).Scan(&shipmentID)

	if err != nil {
		httpError(w, "failed to create shipment", http.StatusInternalServerError)
		return
	}

	// Initial tracking event
	db.Exec(
		`INSERT INTO tracking_events (shipment_id, status, location, description, occurred_at)
		 VALUES ($1, 'label_created', $2, 'Shipping label created', NOW())`,
		shipmentID, req.City,
	)

	publishEvent("shipment.created", map[string]interface{}{
		"shipment_id":     shipmentID,
		"order_id":        req.OrderID,
		"tracking_number": trackingNumber,
		"carrier":         carrier,
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"shipment_id":        shipmentID,
		"tracking_number":    trackingNumber,
		"carrier":            carrier,
		"estimated_delivery": estimatedDelivery.Format("2006-01-02"),
	})
}

func handleShipment(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/shipments/")
	if id == "" {
		httpError(w, "shipment id required", http.StatusBadRequest)
		return
	}

	var s struct {
		ID                int     `json:"id"`
		OrderID           int     `json:"order_id"`
		Carrier           string  `json:"carrier"`
		TrackingNumber    string  `json:"tracking_number"`
		Status            string  `json:"status"`
		RecipientName     string  `json:"recipient_name"`
		City              string  `json:"city"`
		Country           string  `json:"country"`
		WeightKg          float64 `json:"weight_kg"`
		EstimatedDelivery string  `json:"estimated_delivery"`
		CreatedAt         string  `json:"created_at"`
	}

	// Only id, order_id, carrier and status are NOT NULL. The rest scan into
	// Null types, so a NULL column comes back empty rather than as a 404.
	var tracking, recipient, city, country, estDel sql.NullString
	var weight sql.NullFloat64
	err := db.QueryRow(
		`SELECT id, order_id, carrier, tracking_number, status, recipient_name,
		        city, country, weight_kg, estimated_delivery, created_at
		 FROM shipments WHERE id = $1`,
		id,
	).Scan(&s.ID, &s.OrderID, &s.Carrier, &tracking, &s.Status,
		&recipient, &city, &country, &weight, &estDel, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		httpError(w, "shipment not found", http.StatusNotFound)
		return
	}
	if err != nil {
		httpError(w, "query failed", http.StatusInternalServerError)
		return
	}
	s.TrackingNumber, s.RecipientName, s.City, s.Country = tracking.String, recipient.String, city.String, country.String
	s.WeightKg, s.EstimatedDelivery = weight.Float64, estDel.String

	// A shipment for another customer's order gets the same 404 as a missing
	// one. So does a shipment whose order row is gone; any other error is a 500.
	if !privileged(r) {
		var owner string
		err := db.QueryRow("SELECT customer_id FROM orders WHERE id = $1", s.OrderID).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("shipment %s owner lookup error: %v", id, err)
			httpError(w, "query failed", http.StatusInternalServerError)
			return
		}
		if err != nil || !owns(r, owner) {
			httpError(w, "shipment not found", http.StatusNotFound)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s)
}

func handleTrack(w http.ResponseWriter, r *http.Request) {
	tracking := strings.TrimPrefix(r.URL.Path, "/track/")
	if tracking == "" {
		httpError(w, "tracking number required", http.StatusBadRequest)
		return
	}

	var shipmentID int
	var status string
	err := db.QueryRow(
		"SELECT id, status FROM shipments WHERE tracking_number = $1",
		tracking,
	).Scan(&shipmentID, &status)
	if err != nil {
		httpError(w, "tracking number not found", http.StatusNotFound)
		return
	}

	rows, err := db.Query(
		`SELECT status, location, description, occurred_at FROM tracking_events
		 WHERE shipment_id = $1 ORDER BY occurred_at DESC`,
		shipmentID,
	)
	if err != nil {
		httpError(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type Event struct {
		Status      string `json:"status"`
		Location    string `json:"location"`
		Description string `json:"description"`
		OccurredAt  string `json:"occurred_at"`
	}

	events := []Event{}
	for rows.Next() {
		var e Event
		rows.Scan(&e.Status, &e.Location, &e.Description, &e.OccurredAt)
		events = append(events, e)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"tracking_number": tracking,
		"current_status":  status,
		"events":          events,
	})
}

func handleCarrierWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	secret := r.Header.Get("X-Webhook-Secret")
	if webhookSecret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(webhookSecret)) != 1 {
		httpError(w, "invalid webhook secret", http.StatusUnauthorized)
		return
	}

	var req struct {
		TrackingNumber string `json:"tracking_number"`
		Status         string `json:"status"`
		Location       string `json:"location"`
		Description    string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if !webhookStatuses[req.Status] {
		httpError(w, "unknown status", http.StatusBadRequest)
		return
	}

	var shipmentID, orderID int
	err := db.QueryRow(
		"SELECT id, order_id FROM shipments WHERE tracking_number = $1",
		req.TrackingNumber,
	).Scan(&shipmentID, &orderID)
	if err != nil {
		httpError(w, "tracking number not found", http.StatusNotFound)
		return
	}

	// Update shipment status
	db.Exec("UPDATE shipments SET status = $1, updated_at = NOW() WHERE id = $2", req.Status, shipmentID)

	if req.Status == "delivered" {
		db.Exec("UPDATE shipments SET delivered_at = NOW() WHERE id = $1", shipmentID)
	} else if req.Status == "in_transit" {
		db.Exec("UPDATE shipments SET shipped_at = COALESCE(shipped_at, NOW()) WHERE id = $1", shipmentID)
	}

	// Record tracking event
	db.Exec(
		`INSERT INTO tracking_events (shipment_id, status, location, description, occurred_at)
		 VALUES ($1, $2, $3, $4, NOW())`,
		shipmentID, req.Status, req.Location, req.Description,
	)

	publishEvent("shipment."+req.Status, map[string]interface{}{
		"shipment_id":     shipmentID,
		"order_id":        orderID,
		"tracking_number": req.TrackingNumber,
		"status":          req.Status,
		"location":        req.Location,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

func generateTrackingNumber(carrier string) string {
	prefix := "RM"
	switch carrier {
	case "dpd":
		prefix = "DPD"
	case "hermes":
		prefix = "EVR"
	case "ups":
		prefix = "1Z"
	}
	return fmt.Sprintf("%s%d%d", prefix, time.Now().UnixNano(), rand.Intn(100000))
}

func connectServiceBus() {
	queue := os.Getenv("SERVICEBUS_QUEUE")
	connStr := os.Getenv("SERVICEBUS_CONNECTION_STRING")
	namespace := os.Getenv("SERVICEBUS_NAMESPACE")
	if queue == "" || (connStr == "" && namespace == "") {
		log.Println("Service Bus not configured, events will only be logged")
		return
	}

	var err error
	if connStr != "" {
		sbClient, err = azservicebus.NewClientFromConnectionString(connStr, nil)
	} else {
		var cred *azidentity.DefaultAzureCredential
		cred, err = azidentity.NewDefaultAzureCredential(nil)
		if err == nil {
			sbClient, err = azservicebus.NewClient(namespace, cred, nil)
		}
	}
	if err != nil {
		log.Fatalf("Failed to create Service Bus client: %v", err)
	}

	sbSender, err = sbClient.NewSender(queue, nil)
	if err != nil {
		log.Fatalf("Failed to create Service Bus sender: %v", err)
	}
	sbQueue = queue
	log.Printf("Publishing events to Service Bus queue %s", queue)
}

// checkServiceBus attaches a new sender link to the queue and closes it, so
// readiness fails while Service Bus is unreachable. sbSender cannot answer
// this: its open link reports no error until a send fails, even after the
// connection drops. Returns nil when Service Bus is not configured.
func checkServiceBus(ctx context.Context) error {
	if sbClient == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	sender, err := sbClient.NewSender(sbQueue, nil)
	if err != nil {
		return err
	}
	defer sender.Close(ctx)
	_, err = sender.NewMessageBatch(ctx, nil)
	return err
}

func publishEvent(eventType string, payload map[string]interface{}) {
	if sbSender == nil {
		log.Printf("Event (no Service Bus): %s %v", eventType, payload)
		return
	}

	event := map[string]interface{}{
		"type":      eventType,
		"payload":   payload,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	data, _ := json.Marshal(event)

	contentType := "application/json"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := sbSender.SendMessage(ctx, &azservicebus.Message{
		Body:        data,
		Subject:     &eventType,
		ContentType: &contentType,
	}, nil)
	if err != nil {
		log.Printf("Failed to publish event %s: %v", eventType, err)
		return
	}
	log.Printf("Event -> Service Bus: %s", string(data))
}

// privileged reports whether the caller may act on every customer's data: an
// admin, or an internal caller such as the scheduler. api-gateway sets
// X-User-Role from the JWT and only issues customer or admin. Internal callers
// send role service themselves.
func privileged(r *http.Request) bool {
	switch r.Header.Get("X-User-Role") {
	case "admin", "service":
		return true
	}
	return false
}

// owns reports whether the caller may see or change data that belongs to
// customer.
func owns(r *http.Request, customer string) bool {
	return privileged(r) || r.Header.Get("X-User-Email") == customer
}

// requireUser rejects requests without the X-User-Email header that
// api-gateway sets from the JWT. Health checks are exempt.
// The carrier webhook is also exempt: carriers call it without a JWT, and it
// checks its own secret.
func requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/livez", "/healthz", "/webhook":
		default:
			if r.Header.Get("X-User-Email") == "" {
				httpError(w, "missing user identity", http.StatusUnauthorized)
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
