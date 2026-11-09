package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret"

func TestMain(m *testing.M) {
	// The subprocess in TestMainRefusesToStart runs main itself
	if os.Getenv("GATEWAY_TEST_RUN_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	log.SetOutput(io.Discard)
	jwtSecret = []byte(testSecret)
	os.Exit(m.Run())
}

func token(t *testing.T, claims jwt.MapClaims, secret string) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func validToken(t *testing.T) string {
	return token(t, jwt.MapClaims{"sub": "alice@example.com", "role": "customer", "exp": time.Now().Add(time.Hour).Unix()}, testSecret)
}

func setTrustedProxies(t *testing.T, cidrs ...string) {
	t.Helper()
	orig := trustedProxies
	trustedProxies = nil
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		trustedProxies = append(trustedProxies, n)
	}
	t.Cleanup(func() { trustedProxies = orig })
}

// seen is what a fake backend received.
type seen struct {
	Path  string
	Query string
	User  string
	Role  string
}

// fakeBackend records each request and answers 200 with what it saw.
func fakeBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(seen{
			Path:  r.URL.Path,
			Query: r.URL.RawQuery,
			User:  r.Header.Get("X-User-Email"),
			Role:  r.Header.Get("X-User-Role"),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// routeAllTo points every route at one backend, built the way main builds them.
func routeAllTo(t *testing.T, backendURL string) {
	t.Helper()
	for _, v := range []string{"ORDER_SERVICE_URL", "INVENTORY_SERVICE_URL", "PAYMENT_SERVICE_URL", "NOTIFICATION_SERVICE_URL", "SHIPPING_SERVICE_URL"} {
		t.Setenv(v, backendURL)
	}
	orig := routes
	routes = newRoutes(backendURL)
	t.Cleanup(func() { routes = orig })
}

// Pins #125: the gateway exits on startup when JWT_SECRET is unset or empty,
// rather than signing tokens with a known fallback.
func TestMainRefusesToStart(t *testing.T) {
	tests := []struct {
		name    string
		env     []string
		wantLog string
	}{
		{"JWT_SECRET unset", nil, "JWT_SECRET is required"},
		{"JWT_SECRET empty", []string{"JWT_SECRET="}, "JWT_SECRET is required"},
		// With a secret, main gets past the check and stops at the next bad setting
		{"JWT_SECRET set", []string{"JWT_SECRET=x", "TRUSTED_PROXIES=not-a-cidr"}, "invalid TRUSTED_PROXIES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// If main ever starts serving instead, the timeout kills it and the test fails
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "JWT_SECRET=") && !strings.HasPrefix(kv, "TRUSTED_PROXIES=") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			cmd.Env = append(cmd.Env, "GATEWAY_TEST_RUN_MAIN=1", "PORT=0", "REDIS_URL=")
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

// Pins #128: the rate-limit key is the peer address without its port, and
// X-Forwarded-For counts only when the peer is a trusted proxy.
func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		trusted    []string
		remoteAddr string
		xff        []string
		realIP     string
		want       string
	}{
		{name: "port is dropped", remoteAddr: "203.0.113.7:51000", want: "203.0.113.7"},
		{name: "new connection, same key", remoteAddr: "203.0.113.7:51001", want: "203.0.113.7"},
		{name: "ipv6 peer", remoteAddr: "[2001:db8::1]:443", want: "2001:db8::1"},
		{name: "address without port", remoteAddr: "203.0.113.7", want: "203.0.113.7"},
		{name: "XFF ignored from an untrusted peer", remoteAddr: "203.0.113.7:51000", xff: []string{"198.51.100.1"}, want: "203.0.113.7"},
		{name: "XFF ignored with no trusted proxies", trusted: nil, remoteAddr: "10.0.0.5:51000", xff: []string{"198.51.100.1"}, want: "10.0.0.5"},
		{name: "X-Real-IP ignored", remoteAddr: "203.0.113.7:51000", realIP: "198.51.100.1", want: "203.0.113.7"},
		{name: "trusted peer, client from XFF", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.5:51000", xff: []string{"203.0.113.7"}, want: "203.0.113.7"},
		{
			name: "spoofed left-hand entries are ignored", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.5:51000",
			xff: []string{"1.1.1.1, 2.2.2.2, 203.0.113.7"}, want: "203.0.113.7",
		},
		{
			name: "trusted hops on the right are skipped", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.5:51000",
			xff: []string{"1.1.1.1, 203.0.113.7, 10.0.0.9"}, want: "203.0.113.7",
		},
		{
			name: "several XFF headers are read as one list", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.5:51000",
			xff: []string{"1.1.1.1", "203.0.113.7"}, want: "203.0.113.7",
		},
		{name: "trusted peer without XFF", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.5:51000", want: "10.0.0.5"},
		{
			name: "every hop trusted gives the left-most", trusted: []string{"10.0.0.0/8"}, remoteAddr: "10.0.0.5:51000",
			xff: []string{"10.0.0.1, 10.0.0.2"}, want: "10.0.0.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setTrustedProxies(t, tt.trusted...)
			req := httptest.NewRequest(http.MethodGet, "/api/orders/", nil)
			req.RemoteAddr = tt.remoteAddr
			for _, v := range tt.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if tt.realIP != "" {
				req.Header.Set("X-Real-IP", tt.realIP)
			}
			if got := clientIP(req); got != tt.want {
				t.Errorf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

// Pins #128 end to end: a client cannot reset its limit by sending a new
// X-Forwarded-For or opening a new connection.
func TestRateLimit(t *testing.T) {
	mr := miniredis.RunT(t)
	orig := redisClient
	redisClient = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		redisClient.Close()
		redisClient = orig
	})
	setTrustedProxies(t)
	routeAllTo(t, fakeBackend(t).URL)
	auth := "Bearer " + validToken(t)

	send := func(remoteAddr, xff string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/orders/", nil)
		req.RemoteAddr = remoteAddr
		req.Header.Set("X-Forwarded-For", xff)
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		handleProxy(rec, req)
		return rec.Code
	}

	for i := 0; i < 100; i++ {
		// A new port and a new forged address on every request
		addr := fmt.Sprintf("203.0.113.7:%d", 40000+i)
		if code := send(addr, fmt.Sprintf("198.51.100.%d", i)); code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i+1, code)
		}
	}
	if code := send("203.0.113.7:65000", "192.0.2.200"); code != http.StatusTooManyRequests {
		t.Errorf("request 101: status %d, want 429", code)
	}
	if code := send("203.0.113.8:65000", ""); code != http.StatusOK {
		t.Errorf("another client: status %d, want 200", code)
	}
	if ttl := mr.TTL("rate:203.0.113.7"); ttl <= 0 || ttl > time.Minute {
		t.Errorf("rate key TTL = %s, want up to a minute", ttl)
	}
}

func TestHandleProxy(t *testing.T) {
	backend := fakeBackend(t)
	routeAllTo(t, backend.URL)
	tok := validToken(t)

	tests := []struct {
		name      string
		method    string
		path      string
		auth      string
		spoof     bool // client sends its own identity headers
		want      int
		wantSeen  seen
		checkSeen bool
	}{
		{name: "no token", path: "/api/orders/", want: http.StatusUnauthorized},
		{name: "prefix is stripped", path: "/api/orders/42", auth: tok, want: http.StatusOK, checkSeen: true,
			wantSeen: seen{Path: "/42", User: "alice@example.com", Role: "customer"}},
		{name: "bare prefix becomes /", path: "/api/orders", auth: tok, want: http.StatusOK, checkSeen: true,
			wantSeen: seen{Path: "/", User: "alice@example.com", Role: "customer"}},
		{name: "query is kept", path: "/api/orders/?status=shipped", auth: tok, want: http.StatusOK, checkSeen: true,
			wantSeen: seen{Path: "/", Query: "status=shipped", User: "alice@example.com", Role: "customer"}},
		// #126: only the gateway sets the identity headers
		{name: "spoofed identity replaced by the token's", path: "/api/payments/ledger", auth: tok, spoof: true, want: http.StatusOK, checkSeen: true,
			wantSeen: seen{Path: "/ledger", User: "alice@example.com", Role: "customer"}},
		{name: "spoofed identity dropped on a public path", path: "/api/shipping/webhook", spoof: true, want: http.StatusOK, checkSeen: true,
			wantSeen: seen{Path: "/webhook"}},
		{name: "backend health check is public", path: "/api/inventory/healthz", want: http.StatusOK, checkSeen: true,
			wantSeen: seen{Path: "/healthz"}},
		// #122: dashboard-api serves under /dashboard
		{name: "dashboard keeps its /dashboard prefix", path: "/api/dashboard/summary", auth: tok, want: http.StatusOK, checkSeen: true,
			wantSeen: seen{Path: "/dashboard/summary", User: "alice@example.com", Role: "customer"}},
		{name: "dashboard nested path", path: "/api/dashboard/shipping/overview", auth: tok, want: http.StatusOK, checkSeen: true,
			wantSeen: seen{Path: "/dashboard/shipping/overview", User: "alice@example.com", Role: "customer"}},
		{name: "dashboard health check", path: "/api/dashboard/healthz", want: http.StatusOK, checkSeen: true,
			wantSeen: seen{Path: "/dashboard/healthz"}},
		// #129: the dashboard data needs a token like any other route
		{name: "dashboard data without a token", path: "/api/dashboard/summary", want: http.StatusUnauthorized},
		{name: "unknown route", path: "/api/unknown/x", auth: tok, want: http.StatusNotFound},
		{name: "bad token", path: "/api/orders/", auth: "garbage", want: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, tt.path, nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", "Bearer "+tt.auth)
			}
			if tt.spoof {
				req.Header.Set("X-User-Email", "admin@example.com")
				req.Header.Set("X-User-Role", "admin")
			}
			rec := httptest.NewRecorder()
			handleProxy(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.checkSeen {
				var got seen
				if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
					t.Fatalf("decode backend echo: %v", err)
				}
				if got != tt.wantSeen {
					t.Errorf("backend saw %+v, want %+v", got, tt.wantSeen)
				}
			}
		})
	}
}

func TestHandleProxyUpstreamDown(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	routeAllTo(t, down.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/orders/", nil)
	req.Header.Set("Authorization", "Bearer "+validToken(t))
	rec := httptest.NewRecorder()
	handleProxy(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// Pins #122: the dashboard route targets dashboard-api's /dashboard paths.
func TestNewRoutes(t *testing.T) {
	for _, v := range []string{"ORDER_SERVICE_URL", "INVENTORY_SERVICE_URL", "PAYMENT_SERVICE_URL", "NOTIFICATION_SERVICE_URL", "SHIPPING_SERVICE_URL"} {
		t.Setenv(v, "")
	}
	got := newRoutes("http://dashboard-api:8086")
	want := map[string]string{
		"/api/orders":        "http://order-service:8081",
		"/api/inventory":     "http://inventory-service:8082",
		"/api/payments":      "http://payment-service:8083",
		"/api/notifications": "http://notification-service:8084",
		"/api/shipping":      "http://shipping-service:8085",
		"/api/dashboard":     "http://dashboard-api:8086/dashboard",
	}
	if len(got) != len(want) {
		t.Errorf("got %d routes, want %d: %v", len(got), len(want), got)
	}
	for prefix, target := range want {
		if got[prefix] != target {
			t.Errorf("routes[%q] = %q, want %q", prefix, got[prefix], target)
		}
	}
}

// Pins #129: GET / serves the dashboard page from dashboard-api without a
// token. Every other path still goes through the JWT check.
func TestMux(t *testing.T) {
	backend := fakeBackend(t)
	routeAllTo(t, backend.URL)
	mux, err := newMux(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		method   string
		path     string
		want     int
		wantPath string // path the backend saw, if it was called
	}{
		{"dashboard page without a token", http.MethodGet, "/", http.StatusOK, "/"},
		{"other methods on / need a token", http.MethodPost, "/", http.StatusUnauthorized, ""},
		{"other pages need a token", http.MethodGet, "/index.html", http.StatusUnauthorized, ""},
		{"livez", http.MethodGet, "/livez", http.StatusOK, ""},
		{"healthz", http.MethodGet, "/healthz", http.StatusOK, ""},
		{"login is public", http.MethodGet, "/auth/login", http.StatusMethodNotAllowed, ""},
		{"api needs a token", http.MethodGet, "/api/orders/", http.StatusUnauthorized, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.wantPath != "" {
				var got seen
				json.NewDecoder(rec.Body).Decode(&got)
				if got.Path != tt.wantPath || got.User != "" {
					t.Errorf("backend saw %+v, want path %q and no user", got, tt.wantPath)
				}
			}
		})
	}
}

func TestNewMuxBadURL(t *testing.T) {
	if _, err := newMux("://bad"); err == nil {
		t.Error("newMux accepted an invalid dashboard URL")
	}
}

func TestValidateToken(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	tests := []struct {
		name    string
		header  string
		wantErr bool
	}{
		{name: "missing", header: "", wantErr: true},
		{name: "not bearer", header: "Basic dXNlcjpwYXNz", wantErr: true},
		{name: "bearer only", header: "Bearer", wantErr: true},
		{name: "garbage", header: "Bearer not.a.jwt", wantErr: true},
		{name: "wrong secret", header: "Bearer " + token(t, jwt.MapClaims{"sub": "a", "exp": future}, "other-secret"), wantErr: true},
		{name: "expired", header: "Bearer " + token(t, jwt.MapClaims{"sub": "a", "exp": time.Now().Add(-time.Minute).Unix()}, testSecret), wantErr: true},
		{name: "alg none", header: "Bearer " + unsignedToken(t, jwt.MapClaims{"sub": "a", "exp": future}), wantErr: true},
		{name: "valid", header: "Bearer " + token(t, jwt.MapClaims{"sub": "alice@example.com", "exp": future}, testSecret)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/orders/", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			claims, err := validateToken(req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && claims["sub"] != "alice@example.com" {
				t.Errorf("sub = %v", claims["sub"])
			}
		})
	}
}

func unsignedToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuthHandlers(t *testing.T) {
	tests := []struct {
		name     string
		handler  http.HandlerFunc
		method   string
		body     string
		want     int
		wantName string
	}{
		{name: "login wrong method", handler: handleLogin, method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "login invalid json", handler: handleLogin, method: http.MethodPost, body: `{`, want: http.StatusBadRequest},
		{name: "login", handler: handleLogin, method: http.MethodPost, body: `{"email":"alice@example.com","password":"pw"}`, want: http.StatusOK},
		{name: "register wrong method", handler: handleRegister, method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "register invalid json", handler: handleRegister, method: http.MethodPost, body: `{`, want: http.StatusBadRequest},
		{name: "register without password", handler: handleRegister, method: http.MethodPost, body: `{"email":"alice@example.com"}`, want: http.StatusBadRequest},
		{name: "register without email", handler: handleRegister, method: http.MethodPost, body: `{"password":"pw"}`, want: http.StatusBadRequest},
		{name: "register", handler: handleRegister, method: http.MethodPost, body: `{"email":"alice@example.com","password":"pw","name":"Alice"}`, want: http.StatusCreated, wantName: "Alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.handler(rec, httptest.NewRequest(tt.method, "/auth/x", strings.NewReader(tt.body)))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want >= 300 {
				return
			}
			var body struct {
				Token string `json:"token"`
			}
			json.NewDecoder(rec.Body).Decode(&body)

			// The token the gateway issues must pass its own check
			req := httptest.NewRequest(http.MethodGet, "/api/orders/", nil)
			req.Header.Set("Authorization", "Bearer "+body.Token)
			claims, err := validateToken(req)
			if err != nil {
				t.Fatalf("issued token rejected: %v", err)
			}
			if claims["sub"] != "alice@example.com" || claims["role"] != "customer" {
				t.Errorf("claims = %v", claims)
			}
			if tt.wantName != "" && claims["name"] != tt.wantName {
				t.Errorf("name = %v, want %s", claims["name"], tt.wantName)
			}
		})
	}
}

func TestIsPublicPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/healthz", true},
		{"/auth/login", true},
		{"/api/shipping/webhook", true},
		{"/api/orders/healthz", true},
		{"/api/orders/", false},
		{"/api/shipping/shipments", false},
		{"/api/dashboard/summary", false},
		{"/", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isPublicPath(tt.path); got != tt.want {
				t.Errorf("isPublicPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// #19: readiness fails on SIGTERM so the pod leaves the endpoints.
func TestHandleHealth(t *testing.T) {
	tests := []struct {
		name         string
		shuttingDown bool
		want         int
		wantStatus   string
	}{
		{"serving", false, http.StatusOK, "ok"},
		{"shutting down", true, http.StatusServiceUnavailable, "shutting down"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shuttingDown.Store(tt.shuttingDown)
			t.Cleanup(func() { shuttingDown.Store(false) })
			rec := httptest.NewRecorder()
			handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			var body map[string]string
			json.NewDecoder(rec.Body).Decode(&body)
			if rec.Code != tt.want || body["status"] != tt.wantStatus {
				t.Errorf("status = %d %q, want %d %q", rec.Code, body["status"], tt.want, tt.wantStatus)
			}
		})
	}
}
