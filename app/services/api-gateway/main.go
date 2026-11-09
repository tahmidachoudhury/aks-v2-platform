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
	"sync"
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
)

func main() {
	jwtSecret = []byte(os.Getenv("JWT_SECRET"))
	if len(jwtSecret) == 0 {
		log.Fatal("JWT_SECRET is required")
	}

	dashboardURL := getEnv("DASHBOARD_SERVICE_URL", "http://dashboard-api:8086")

	// Service routes - internal service URLs
	routes = map[string]string{
		"/api/orders":        getEnv("ORDER_SERVICE_URL", "http://order-service:8081"),
		"/api/inventory":     getEnv("INVENTORY_SERVICE_URL", "http://inventory-service:8082"),
		"/api/payments":      getEnv("PAYMENT_SERVICE_URL", "http://payment-service:8083"),
		"/api/notifications": getEnv("NOTIFICATION_SERVICE_URL", "http://notification-service:8084"),
		"/api/shipping":      getEnv("SHIPPING_SERVICE_URL", "http://shipping-service:8085"),
		// dashboard-api serves under /dashboard. The proxy joins the target's path
		// to the stripped one, so /api/dashboard/summary reaches /dashboard/summary.
		"/api/dashboard": dashboardURL + "/dashboard",
	}

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
		log.Fatalf("invalid DASHBOARD_SERVICE_URL: %v", err)
	}
	mux.Handle("GET /{$}", httputil.NewSingleHostReverseProxy(dashboard))
	mux.HandleFunc("/", handleProxy)

	port := getEnv("PORT", "8080")
	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go gracefulShutdown(server)

	log.Printf("API Gateway listening on :%s", port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "api-gateway"})
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
		"sub":   req.Email,
		"role":  "customer",
		"exp":   time.Now().Add(24 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
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
		"role": "customer",
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

	// Find matching route
	for prefix, targetURL := range routes {
		if strings.HasPrefix(r.URL.Path, prefix) {
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
			r.Header.Set("X-Forwarded-For", r.RemoteAddr)
			r.Header.Set("X-Request-ID", fmt.Sprintf("%d", time.Now().UnixNano()))

			proxy.ServeHTTP(w, r)
			return
		}
	}

	httpError(w, "not found", http.StatusNotFound)
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

var shutdownOnce sync.Once

func gracefulShutdown(server *http.Server) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	shutdownOnce.Do(func() {
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	})
}


