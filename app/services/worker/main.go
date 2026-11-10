package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
)

// Event represents a message from Service Bus
type Event struct {
	Type      string                 `json:"type"`
	Payload   map[string]interface{} `json:"payload"`
	Timestamp string                 `json:"timestamp"`
}

func main() {
	queue := requireEnv("SERVICEBUS_QUEUE")

	// Internal service URLs for event-driven calls
	services := map[string]string{
		"inventory":    requireEnv("INVENTORY_SERVICE_URL"),
		"payment":      requireEnv("PAYMENT_SERVICE_URL"),
		"notification": requireEnv("NOTIFICATION_SERVICE_URL"),
		"shipping":     requireEnv("SHIPPING_SERVICE_URL"),
		"order":        requireEnv("ORDER_SERVICE_URL"),
	}

	sbClient, err := newServiceBusClient()
	if err != nil {
		log.Fatalf("Failed to create Service Bus client: %v", err)
	}
	receiver, err := sbClient.NewReceiverForQueue(queue, &azservicebus.ReceiverOptions{
		ReceiveMode: azservicebus.ReceiveModePeekLock,
	})
	if err != nil {
		log.Fatalf("Failed to create Service Bus receiver: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Health check endpoint
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "worker"})
		})
		port := getEnv("HEALTH_PORT", "8090")
		log.Printf("Worker health check on :%s", port)
		http.ListenAndServe(":"+port, mux)
	}()

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Shutting down worker...")
		cancel()
	}()

	log.Printf("Worker started, receiving events from Service Bus queue %s...", queue)
	pollAndProcess(ctx, receiver, services)

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	receiver.Close(closeCtx)
	sbClient.Close(closeCtx)
}

func newServiceBusClient() (*azservicebus.Client, error) {
	if connStr := os.Getenv("SERVICEBUS_CONNECTION_STRING"); connStr != "" {
		return azservicebus.NewClientFromConnectionString(connStr, nil)
	}
	namespace := os.Getenv("SERVICEBUS_NAMESPACE")
	if namespace == "" {
		return nil, errors.New("SERVICEBUS_CONNECTION_STRING or SERVICEBUS_NAMESPACE is required")
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	return azservicebus.NewClient(namespace, cred, nil)
}

func pollAndProcess(ctx context.Context, receiver *azservicebus.Receiver, services map[string]string) {
	client := &http.Client{Timeout: 10 * time.Second}

	for {
		messages, err := receiver.ReceiveMessages(ctx, 10, nil)
		if ctx.Err() != nil {
			// Release anything received during shutdown so it is redelivered straight away
			for _, msg := range messages {
				abandon(receiver, msg)
			}
			log.Println("Worker stopped")
			return
		}
		if err != nil {
			log.Printf("Failed to receive messages: %v", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			continue
		}

		for _, msg := range messages {
			var event Event
			if err := json.Unmarshal(msg.Body, &event); err != nil {
				log.Printf("Failed to parse event: %v", err)
				abandon(receiver, msg)
				continue
			}

			log.Printf("Processing event: %s", event.Type)

			if err := handleEvent(client, services, event); err != nil {
				log.Printf("Failed to handle event %s: %v", event.Type, err)
				// Abandon so Service Bus redelivers, then dead-letters after max delivery count
				abandon(receiver, msg)
				continue
			}

			if err := receiver.CompleteMessage(context.Background(), msg, nil); err != nil {
				log.Printf("Failed to complete message %s: %v", msg.MessageID, err)
				continue
			}
			log.Printf("Successfully processed: %s", event.Type)
		}
	}
}

func abandon(receiver *azservicebus.Receiver, msg *azservicebus.ReceivedMessage) {
	if err := receiver.AbandonMessage(context.Background(), msg, nil); err != nil {
		log.Printf("Failed to abandon message %s: %v", msg.MessageID, err)
		return
	}
	log.Printf("Abandoned message %s (delivery %d)", msg.MessageID, msg.DeliveryCount)
}

func handleEvent(client *http.Client, services map[string]string, event Event) error {
	switch event.Type {

	case "order.created":
		// 1. Reserve inventory
		log.Printf("  -> Reserving inventory for order")
		// POST to inventory-service/reserve with order items
		// If reservation fails, update order status to "cancelled"

		// 2. Process payment
		log.Printf("  -> Processing payment")
		// POST to payment-service/charge
		// If payment fails, release inventory reservation

		// 3. Send confirmation notification
		log.Printf("  -> Sending order confirmation")
		// POST to notification-service/send with order_confirmed template

		// 4. Update order to confirmed
		log.Printf("  -> Confirming order")
		// PUT to order-service/status with new_status: "confirmed"

	case "order.status_changed":
		newStatus, _ := event.Payload["new_status"].(string)

		switch newStatus {
		case "processing":
			// Create shipment
			log.Printf("  -> Creating shipment for order")
			// POST to shipping-service/shipments

		case "shipped":
			// Notify customer
			log.Printf("  -> Sending shipping notification")
			// POST to notification-service/send with order_shipped template

		case "delivered":
			log.Printf("  -> Sending delivery notification")
			// POST to notification-service/send with order_delivered template

		case "cancelled":
			// Release inventory
			log.Printf("  -> Releasing inventory reservation")
			// POST to inventory-service/release

			// Process refund if payment was made
			log.Printf("  -> Processing refund")
			// POST to payment-service/refund
		}

	case "payment.completed":
		log.Printf("  -> Payment successful, confirming order")
		// Update order status to confirmed

	case "payment.failed":
		log.Printf("  -> Payment failed, cancelling order")
		// Release inventory reservation
		// Update order status to cancelled
		// Send payment failed notification

	case "shipment.created":
		log.Printf("  -> Shipment created, updating order to processing")
		// Update order status

	case "shipment.delivered":
		log.Printf("  -> Shipment delivered, updating order")
		// Update order status to delivered
		// Send delivery notification

	default:
		log.Printf("  -> Unknown event type: %s (skipping)", event.Type)
	}

	_ = client
	_ = services
	return nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// requireEnv returns the value of key, or exits if it is unset or empty.
func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s is required", key)
	}
	return v
}
