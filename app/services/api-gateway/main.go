package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/golang-jwt/jwt/v5"
)

var (
	redisClient *redis.Client
	ctx         = context.Background()
	jwtSecret   []byte
	routes      map[string]string

	// Proxies whose X-Forwarded-For the rate limit trusts, from TRUSTED_PROXIES
	trustedProxies []*net.IPNet

	// Emails whose tokens carry role admin, from ADMIN_EMAILS
	adminEmails map[string]bool

	// Set on SIGTERM so /healthz fails and the pod leaves the Service endpoints
	shuttingDown atomic.Bool
)

func main() {
	jwtSecret = []byte(requireEnv("JWT_SECRET"))

	adminEmails = parseEmails(os.Getenv("ADMIN_EMAILS"))

	dashboardURL := requireEnv("DASHBOARD_SERVICE_URL")
	routes = newRoutes(dashboardURL)

	// Comma-separated CIDRs, such as Traefik's pod range. Empty means trust no proxy.
	for _, cidr := range strings.Split(os.Getenv("TRUSTED_PROXIES"), ",") {
		if cidr = strings.TrimSpace(cidr); cidr == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			log.Fatalf("invalid TRUSTED_PROXIES entry %q: %v", cidr, err)
		}
		trustedProxies = append(trustedProxies, ipNet)
	}

	// Redis for rate limiting
	redisURL := os.Getenv("REDIS_URL")
	if redisURL != "" {
		opt, err := redis.ParseURL(redisURL)
		if err != nil {
			log.Printf("WARNING: invalid REDIS_URL, rate limiting disabled: %v", err)
		} else {
			redisClient = redis.NewClient(opt)
			if _, err := redisClient.Ping(ctx).Result(); err != nil {
				log.Printf("WARNING: Redis not reachable, rate limiting disabled: %v", err)
				redisClient = nil
			} else {
				log.Println("Redis connected for rate limiting")
			}
		}
	}

	mux, err := newMux(dashboardURL)
	if err != nil {
		log.Fatalf("invalid DASHBOARD_SERVICE_URL: %v", err)
	}

	port := getEnv("PORT", "8080")
	drainDelay, err := time.ParseDuration(getEnv("SHUTDOWN_DRAIN_DELAY", "5s"))
	if err != nil {
		log.Fatalf("invalid SHUTDOWN_DRAIN_DELAY: %v", err)
	}
	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("API Gateway listening on :%s", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	// Fail readiness and keep serving while the pod leaves the Service endpoints,
	// then stop accepting connections and let in-flight requests finish. main
	// waits for Shutdown, so the process does not exit under them.
	log.Printf("Shutting down, draining for %s", drainDelay)
	shuttingDown.Store(true)
	time.Sleep(drainDelay)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown error: %v", err)
	}
	if redisClient != nil {
		redisClient.Close()
	}
}

// newRoutes maps each /api prefix to the internal URL of its service.
func newRoutes(dashboardURL string) map[string]string {
	return map[string]string{
		"/api/orders":        requireEnv("ORDER_SERVICE_URL"),
		"/api/inventory":     requireEnv("INVENTORY_SERVICE_URL"),
		"/api/payments":      requireEnv("PAYMENT_SERVICE_URL"),
		"/api/notifications": requireEnv("NOTIFICATION_SERVICE_URL"),
		"/api/shipping":      requireEnv("SHIPPING_SERVICE_URL"),
		// dashboard-api serves under /dashboard. The proxy joins the target's path
		// to the stripped one, so /api/dashboard/summary reaches /dashboard/summary.
		"/api/dashboard": dashboardURL + "/dashboard",
	}
}

func newMux(dashboardURL string) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/auth/login", handleLogin)
	mux.HandleFunc("/auth/register", handleRegister)

	// The dashboard UI page, served by dashboard-api, without a JWT. This puts
	// it on the same origin as /auth and /api, where it sends its calls. The
	// page holds no data: its /api/dashboard calls go through handleProxy.
	dashboard, err := url.Parse(dashboardURL)
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /{$}", httputil.NewSingleHostReverseProxy(dashboard))
	mux.HandleFunc("/", handleProxy)
	return mux, nil
}

// handleHealth reports Redis but never fails on it: without Redis the gateway
// still serves, only without rate limiting. The Redis ping is bounded at
// 300ms so the handler answers well inside the kubelet's 1s probe timeout.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body := map[string]string{"status": "ok", "service": "api-gateway"}
	if shuttingDown.Load() {
		body["status"] = "shutting down"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		body["redis"] = redisState(r.Context())
	}
	json.NewEncoder(w).Encode(body)
}

// redisState is "disabled" without REDIS_URL, "degraded" while rate limiting
// is off and "ok" while Redis answers. redisClient is nil after a failed ping
// at startup, which leaves rate limiting off until the gateway restarts.
func redisState(ctx context.Context) string {
	if os.Getenv("REDIS_URL") == "" {
		return "disabled"
	}
	if redisClient == nil {
		return "degraded"
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return "degraded"
	}
	return "ok"
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// In production this would validate against a user database
	// For this project, accept any email/password and return a JWT
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  req.Email,
		"role": roleFor(req.Email),
		"exp":  time.Now().Add(24 * time.Hour).Unix(),
		"iat":  time.Now().Unix(),
	})

	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		httpError(w, "failed to generate token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": tokenString})
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Email == "" || req.Password == "" {
		httpError(w, "email and password required", http.StatusBadRequest)
		return
	}

	// Return a JWT for the new user
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  req.Email,
		"name": req.Name,
		"role": roleFor(req.Email),
		"exp":  time.Now().Add(24 * time.Hour).Unix(),
		"iat":  time.Now().Unix(),
	})

	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		httpError(w, "failed to generate token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "registered",
		"email":   req.Email,
		"token":   tokenString,
	})
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	// Rate limiting
	if redisClient != nil {
		ip := clientIP(r)
		key := fmt.Sprintf("rate:%s", ip)
		count, _ := redisClient.Incr(ctx, key).Result()
		if count == 1 {
			redisClient.Expire(ctx, key, time.Minute)
		}
		limit := 100 // requests per minute
		if count > int64(limit) {
			httpError(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	// Only the gateway sets the identity headers. Drop any the client sent.
	r.Header.Del("X-User-Email")
	r.Header.Del("X-User-Role")

	// Auth check (skip for public endpoints)
	if !isPublicPath(r.URL.Path) {
		claims, err := validateToken(r)
		if err != nil {
			httpError(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		// Forward user info to downstream services
		if sub, ok := claims["sub"].(string); ok {
			r.Header.Set("X-User-Email", sub)
		}
		if role, ok := claims["role"].(string); ok {
			r.Header.Set("X-User-Role", role)
		}
	}

	// Find matching route. A prefix matches only up to a "/", so
	// /api/ordersfoo does not reach order-service.
	for prefix, targetURL := range routes {
		if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
			target, err := url.Parse(targetURL)
			if err != nil {
				httpError(w, "bad upstream config", http.StatusInternalServerError)
				return
			}

			proxy := httputil.NewSingleHostReverseProxy(target)
			proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
				log.Printf("Proxy error: %v", err)
				httpError(w, "service unavailable", http.StatusBadGateway)
			}

			// Strip the route prefix for downstream
			r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
			r.Header.Set("X-Request-ID", fmt.Sprintf("%d", time.Now().UnixNano()))

			proxy.ServeHTTP(w, r)
			return
		}
	}

	httpError(w, "not found", http.StatusNotFound)
}

// parseEmails reads a comma-separated email list into a set, lower-cased.
func parseEmails(list string) map[string]bool {
	emails := map[string]bool{}
	for _, e := range strings.Split(list, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			emails[e] = true
		}
	}
	return emails
}

// roleFor is the role a token for email carries: admin for an address in
// ADMIN_EMAILS, otherwise customer. The gateway never issues role service;
// only internal callers send it, directly to the backends.
func roleFor(email string) string {
	if adminEmails[strings.ToLower(strings.TrimSpace(email))] {
		return "admin"
	}
	return "customer"
}

func isPublicPath(path string) bool {
	public := []string{"/healthz", "/auth/", "/api/shipping/webhook"}
	for _, p := range public {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	// Allow health checks through to downstream services
	if strings.HasSuffix(path, "/healthz") {
		return true
	}
	return false
}

func validateToken(r *http.Request) (jwt.MapClaims, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return nil, fmt.Errorf("missing Authorization header")
	}

	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return nil, fmt.Errorf("invalid Authorization format")
	}

	token, err := jwt.Parse(parts[1], func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return jwtSecret, nil
	})
	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		return claims, nil
	}
	return nil, fmt.Errorf("invalid token")
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

// requireEnv returns the value of key, or exits if it is unset or empty.
func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s is required", key)
	}
	return v
}

// clientIP returns the address the rate limit counts against. It reads
// X-Forwarded-For only when the direct peer is a trusted proxy, and then takes
// the right-most address that is not a trusted proxy: the one the nearest
// proxy saw. Anything a client writes into the header sits to the left of
// that, so it cannot change the result.
func clientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if !isTrustedProxy(ip) {
		return ip
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		if !isTrustedProxy(hop) {
			return hop
		}
		ip = hop
	}
	return ip
}

func isTrustedProxy(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	for _, ipNet := range trustedProxies {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
}
