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
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// The subprocess in TestMainRefusesToStart runs main itself
	if os.Getenv("WORKER_TEST_RUN_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// The settings main exits without, as key=value
var requiredEnv = []string{
	"SERVICEBUS_QUEUE=order-events",
	"ORDER_SERVICE_URL=http://order-service:8081",
	"INVENTORY_SERVICE_URL=http://inventory-service:8082",
	"PAYMENT_SERVICE_URL=http://payment-service:8083",
	"NOTIFICATION_SERVICE_URL=http://notification-service:8084",
	"SHIPPING_SERVICE_URL=http://shipping-service:8085",
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

// Pins #18: the worker exits on startup when the queue or a service URL is
// unset, rather than falling back to a built-in value.
func TestMainRefusesToStart(t *testing.T) {
	type test struct {
		name    string
		env     []string
		wantLog string
	}
	tests := []test{
		// With every setting but a connection, main stops at the Service Bus client
		{"all set", withoutEnv(""), "SERVICEBUS_CONNECTION_STRING or SERVICEBUS_NAMESPACE is required"},
	}
	for _, kv := range requiredEnv {
		key, _, _ := strings.Cut(kv, "=")
		tests = append(tests, test{key + " unset", withoutEnv(key), key + " is required"})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// If main ever starts receiving instead, the timeout kills it and the test fails
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
			for _, kv := range os.Environ() {
				key, _, _ := strings.Cut(kv, "=")
				if !strings.HasSuffix(key, "_SERVICE_URL") && !strings.HasPrefix(key, "SERVICEBUS_") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			cmd.Env = append(cmd.Env, "WORKER_TEST_RUN_MAIN=1", "HEALTH_PORT=0")
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

// The publishers send {type, payload, timestamp}. The worker must read the
// same shape.
func TestEventParsing(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantType  string
		wantOrder float64
		wantErr   bool
	}{
		{
			name:      "order-service event",
			body:      `{"type":"order.created","payload":{"order_id":7,"customer_id":"alice@example.com","total":25.5},"timestamp":"2026-11-09T12:00:00Z"}`,
			wantType:  "order.created",
			wantOrder: 7,
		},
		{
			name:      "shipping-service event",
			body:      `{"type":"shipment.delivered","payload":{"shipment_id":3,"order_id":9,"status":"delivered"},"timestamp":"2026-11-09T12:00:00Z"}`,
			wantType:  "shipment.delivered",
			wantOrder: 9,
		},
		{name: "no payload", body: `{"type":"payment.failed"}`, wantType: "payment.failed"},
		{name: "not json", body: `order.created`, wantErr: true},
		{name: "payload not an object", body: `{"type":"order.created","payload":[1,2]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var event Event
			err := json.Unmarshal([]byte(tt.body), &event)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if event.Type != tt.wantType {
				t.Errorf("type = %q, want %q", event.Type, tt.wantType)
			}
			if got, _ := event.Payload["order_id"].(float64); got != tt.wantOrder {
				t.Errorf("order_id = %v, want %v", got, tt.wantOrder)
			}
		})
	}
}

// The handlers are log-only stubs: each event is accepted, so the message is
// completed, and no downstream service is called yet. When a handler starts
// calling services, this test needs the fake backends to answer.
func TestHandleEvent(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	services := map[string]string{
		"inventory":    backend.URL,
		"payment":      backend.URL,
		"notification": backend.URL,
		"shipping":     backend.URL,
		"order":        backend.URL,
	}
	client := &http.Client{Timeout: time.Second}

	tests := []struct {
		name  string
		event Event
	}{
		{"order created", Event{Type: "order.created", Payload: map[string]interface{}{"order_id": 7.0}}},
		{"order processing", Event{Type: "order.status_changed", Payload: map[string]interface{}{"new_status": "processing"}}},
		{"order shipped", Event{Type: "order.status_changed", Payload: map[string]interface{}{"new_status": "shipped"}}},
		{"order delivered", Event{Type: "order.status_changed", Payload: map[string]interface{}{"new_status": "delivered"}}},
		{"order cancelled", Event{Type: "order.status_changed", Payload: map[string]interface{}{"new_status": "cancelled"}}},
		{"status change with no status", Event{Type: "order.status_changed"}},
		{"status change with a non-string status", Event{Type: "order.status_changed", Payload: map[string]interface{}{"new_status": 3.0}}},
		{"payment completed", Event{Type: "payment.completed"}},
		{"payment failed", Event{Type: "payment.failed"}},
		{"shipment created", Event{Type: "shipment.created"}},
		{"shipment delivered", Event{Type: "shipment.delivered"}},
		{"shipment in transit is skipped", Event{Type: "shipment.in_transit"}},
		{"payment refunded is skipped", Event{Type: "payment.refunded"}},
		{"unknown type is skipped", Event{Type: "something.else"}},
		{"empty type is skipped", Event{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := handleEvent(client, services, tt.event); err != nil {
				t.Errorf("handleEvent(%s) = %v, want nil", tt.event.Type, err)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("handlers made %d service calls, want 0 while they are stubs", n)
	}
}

func TestNewServiceBusClient(t *testing.T) {
	tests := []struct {
		name      string
		connStr   string
		namespace string
		wantErr   bool
	}{
		{name: "nothing configured", wantErr: true},
		// The client connects lazily, so a well-formed string is enough
		{name: "emulator connection string", connStr: "Endpoint=sb://localhost;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=key;UseDevelopmentEmulator=true"},
		{name: "malformed connection string", connStr: "not-a-connection-string", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SERVICEBUS_CONNECTION_STRING", tt.connStr)
			t.Setenv("SERVICEBUS_NAMESPACE", tt.namespace)
			client, err := newServiceBusClient()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil {
				client.Close(context.Background())
			}
		})
	}
}

func TestGetEnv(t *testing.T) {
	tests := []struct {
		name, value, want string
	}{
		{"unset uses the fallback", "", "http://order-service:8081"},
		{"set wins", "http://localhost:9000", "http://localhost:9000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORDER_SERVICE_URL", tt.value)
			if got := getEnv("ORDER_SERVICE_URL", "http://order-service:8081"); got != tt.want {
				t.Errorf("getEnv = %q, want %q", got, tt.want)
			}
		})
	}
}
