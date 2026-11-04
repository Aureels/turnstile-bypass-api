package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/patrickmn/go-cache"
	"golang.org/x/net/proxy"
	"golang.org/x/time/rate"
)

const (
	Version         = "1.0.0"
	DefaultPort     = ":8080"
	TurnstileVerify = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	UserAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	MaxRetries      = 3
	RequestTimeout  = 30 * time.Second
	ProxyTimeout    = 15 * time.Second
	CacheTTL        = 10 * time.Minute
	CleanupInterval = 5 * time.Minute
	MaxConcurrent   = 1000
)

var (
	ErrNoProxies     = errors.New("no proxies available")
	ErrSolvingFailed = errors.New("turnstile solving failed")
)

type Config struct {
	Port            string
	ProxiesFile     string
	SiteKey         string
	TargetURL       string
	MaxWorkers      int
	RateLimit       int
	BurstLimit      int
	EnableMetrics   bool
	TwoCaptchaKey   string
	CapMonsterKey   string
	CustomSolverURL string
}

type Proxy struct {
	Address  string
	Username string
	Password string
	Scheme   string
	Alive    bool
	Latency  time.Duration
	LastUsed time.Time
	Failures int
	listed   bool
	mu       sync.RWMutex
}

type ProxyPool struct {
	mu      sync.RWMutex
	proxies []*Proxy
	alive   []*Proxy
	idx     uint64
}

type TurnstileTask struct {
	ID           string
	SiteKey      string
	PageURL      string
	Action       string
	CData        string
	Proxy        *Proxy
	CreatedAt    time.Time
	Result       string
	Error        error
	Attempts     int
	SolverUsed   string
	Status       string
	Fetch        bool
	FetchMethod  string
	FetchBody    string
	FetchHeaders map[string]string
	PageContent  string
	ContentType  string
	StatusCode   int
	Headers      map[string]string
}

type SolverResult struct {
	Token       string
	SolverUsed  string
	Latency     time.Duration
	ProxyUsed   string
	Error       error
	PageContent string
	ContentType string
	StatusCode  int
	Headers     map[string]string
}

type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Meta    interface{} `json:"meta,omitempty"`
}

type SolveRequest struct {
	SiteKey      string            `json:"sitekey"`
	PageURL      string            `json:"pageurl"`
	Action       string            `json:"action,omitempty"`
	CData        string            `json:"cdata,omitempty"`
	Proxy        string            `json:"proxy,omitempty"`
	Priority     int               `json:"priority,omitempty"`
	Async        bool              `json:"async,omitempty"`
	CallbackURL  string            `json:"callback_url,omitempty"`
	Fetch        bool              `json:"fetch,omitempty"`
	FetchMethod  string            `json:"fetch_method,omitempty"`
	FetchBody    string            `json:"fetch_body,omitempty"`
	FetchHeaders map[string]string `json:"fetch_headers,omitempty"`
}

type SolveResponse struct {
	TaskID      string            `json:"task_id"`
	Token       string            `json:"token,omitempty"`
	Status      string            `json:"status"`
	SolverUsed  string            `json:"solver_used,omitempty"`
	LatencyMs   int64             `json:"latency_ms,omitempty"`
	ProxyUsed   string            `json:"proxy_used,omitempty"`
	PageContent string            `json:"page_content,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	StatusCode  int               `json:"status_code,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

type Metrics struct {
	TotalRequests     uint64
	SuccessfulSolves  uint64
	FailedSolves      uint64
	ActiveWorkers     int32
	ProxyCount        int
	AliveProxies      int
	AvgLatencyMs      int64
	RequestsPerSecond float64
	StartTime         time.Time
	mu                sync.RWMutex
}

var (
	cfg          Config
	pool         *ProxyPool
	taskCache    *cache.Cache
	metrics      *Metrics
	limiter      *rate.Limiter
	workerSem    chan struct{}
	solverClient *http.Client
)

func main() {
	cfg = loadConfig()
	initLogger()
	initMetrics()
	initProxyPool()
	initHTTPClients()
	initCache()
	initRateLimiter()
	initWorkers()

	r := setupRouter()
	srv := &http.Server{
		Addr:         cfg.Port,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logFatal("server failed: %v", err)
		}
	}()

	logInfo("Turnstile Bypass API v%s started on %s", Version, cfg.Port)
	logInfo("Proxies: %d | Workers: %d | SiteKey default: %s | Target default: %s",
		len(pool.proxies), cfg.MaxWorkers, orNone(cfg.SiteKey), orNone(cfg.TargetURL))

	waitForShutdown(srv)
}

func loadConfig() Config {
	return Config{
		Port:            getEnv("PORT", DefaultPort),
		ProxiesFile:     getEnv("PROXIES_FILE", "proxies.txt"),
		SiteKey:         getEnv("SITE_KEY", ""),
		TargetURL:       getEnv("TARGET_URL", ""),
		MaxWorkers:      getEnvInt("MAX_WORKERS", MaxConcurrent),
		RateLimit:       getEnvInt("RATE_LIMIT", 100),
		BurstLimit:      getEnvInt("BURST_LIMIT", 200),
		EnableMetrics:   getEnvBool("ENABLE_METRICS", true),
		TwoCaptchaKey:   getEnv("TWOCAPTCHA_KEY", ""),
		CapMonsterKey:   getEnv("CAPMONSTER_KEY", ""),
		CustomSolverURL: getEnv("CUSTOM_SOLVER_URL", ""),
	}
}

func orNone(s string) string {
	if s == "" {
		return "(per-request)"
	}
	return s
}

func resolveTarget(req *SolveRequest) error {
	if req.SiteKey == "" {
		req.SiteKey = cfg.SiteKey
	}
	if req.PageURL == "" {
		req.PageURL = cfg.TargetURL
	}

	if req.SiteKey == "" {
		return errors.New("sitekey is required")
	}
	if req.PageURL == "" {
		return errors.New("pageurl is required")
	}
	u, err := url.Parse(req.PageURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("pageurl must be an absolute URL, got %q", req.PageURL)
	}
	return nil
}

func initLogger() {
	gin.SetMode(gin.ReleaseMode)
}

func initMetrics() {
	metrics = &Metrics{StartTime: time.Now()}
	go metricsCollector()
}

func initProxyPool() {
	pool = &ProxyPool{}
	loadProxies(cfg.ProxiesFile)
	if getEnvInt("HEALTH_CHECK_INTERVAL", 300) > 0 {
		go func() {
			runHealthSweep()
			proxyHealthChecker()
		}()
	}
}

func loadProxies(file string) {
	data, err := os.ReadFile(file)
	if err != nil {
		logWarn("proxies file not found: %v, running without proxies", err)
		return
	}

	seen := make(map[string]bool)
	var loaded []*Proxy
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := parseProxy(line)
		if p == nil || seen[p.Scheme+"://"+p.Address] {
			continue
		}
		seen[p.Scheme+"://"+p.Address] = true
		loaded = append(loaded, p)
	}

	pool.mu.Lock()
	pool.proxies = loaded
	pool.alive = pool.alive[:0]
	atomic.StoreUint64(&pool.idx, 0)
	for _, p := range loaded {
		p.mu.Lock()
		p.Alive = true
		p.Failures = 0
		p.listed = true
		p.mu.Unlock()
		pool.alive = append(pool.alive, p)
	}
	pool.mu.Unlock()

	logInfo("Loaded %d proxies from %s", len(loaded), file)
}

func parseProxy(raw string) *Proxy {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil
	}

	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
	default:
		return nil
	}

	p := &Proxy{
		Address:  u.Host,
		Scheme:   scheme,
		Alive:    true,
		LastUsed: time.Now(),
	}
	if u.User != nil {
		p.Username = u.User.Username()
		p.Password, _ = u.User.Password()
	}
	return p
}

func (p *ProxyPool) Get() *Proxy {
	p.mu.RLock()
	n := len(p.alive)
	if n == 0 {
		p.mu.RUnlock()
		return nil
	}
	pr := p.alive[atomic.AddUint64(&p.idx, 1)%uint64(n)]
	p.mu.RUnlock()

	pr.mu.Lock()
	pr.LastUsed = time.Now()
	pr.mu.Unlock()
	return pr
}

func (p *ProxyPool) MarkDead(pr *Proxy) {
	pr.mu.Lock()
	pr.Failures++
	fatal := pr.Failures >= 5
	if fatal {
		pr.Alive = false
	}
	pr.mu.Unlock()

	if fatal {
		p.removeAlive(pr)
	}
}

func (p *ProxyPool) MarkAlive(pr *Proxy, latency time.Duration) {
	pr.mu.Lock()
	pr.Failures = 0
	pr.Alive = true
	pr.Latency = latency
	pr.mu.Unlock()

	p.addAlive(pr)
}

func (p *ProxyPool) addAlive(pr *Proxy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr.listed {
		return
	}
	pr.listed = true
	p.alive = append(p.alive, pr)
}

func (p *ProxyPool) removeAlive(pr *Proxy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !pr.listed {
		return
	}
	pr.listed = false
	for i, x := range p.alive {
		if x == pr {
			last := len(p.alive) - 1
			p.alive[i] = p.alive[last]
			p.alive = p.alive[:last]
			return
		}
	}
}

func (p *ProxyPool) Stats() (total, alive int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies), len(p.alive)
}

func proxyHealthChecker() {
	interval := getEnvInt("HEALTH_CHECK_INTERVAL", 300)
	if interval <= 0 {
		logInfo("proxy health checker disabled")
		return
	}

	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		runHealthSweep()
	}
}

func runHealthSweep() {
	pool.mu.RLock()
	proxies := make([]*Proxy, len(pool.proxies))
	copy(proxies, pool.proxies)
	pool.mu.RUnlock()

	if len(proxies) == 0 {
		return
	}

	concurrency := getEnvInt("HEALTH_CHECK_CONCURRENCY", 300)
	if concurrency < 1 {
		concurrency = 1
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)

	for _, pr := range proxies {
		wg.Add(1)
		sem <- struct{}{}
		go func(p *Proxy) {
			defer wg.Done()
			defer func() { <-sem }()
			checkProxyHealth(p)
		}(pr)
	}
	wg.Wait()

	total, alive := pool.Stats()
	metrics.mu.Lock()
	metrics.ProxyCount = total
	metrics.AliveProxies = alive
	metrics.mu.Unlock()
	logInfo("Proxy health sweep: %d/%d alive", alive, total)
}

func checkProxyHealth(p *Proxy) {
	start := time.Now()
	client := buildHTTPClient(p, ProxyTimeout)
	resp, err := client.Get("http://www.gstatic.com/generate_204")
	latency := time.Since(start)

	if err != nil || resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 400 {
		pool.MarkDead(p)
		return
	}
	resp.Body.Close()
	pool.MarkAlive(p, latency)
}

func initHTTPClients() {
	jar, _ := cookiejar.New(nil)
	solverClient = &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 50,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: RequestTimeout,
	}
}

func buildHTTPClient(p *Proxy, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}

	if p != nil {
		switch p.Scheme {
		case "socks5", "socks5h":

			if p.Scheme == "socks5" && p.Username == "" {
				u, _ := url.Parse(fmt.Sprintf("socks5://%s", p.Address))
				transport.Proxy = http.ProxyURL(u)
				break
			}
			dialer, err := proxy.SOCKS5("tcp", p.Address, &proxy.Auth{
				User:     p.Username,
				Password: p.Password,
			}, &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second})
			if err == nil {
				transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				}
			}
		case "socks4", "socks4a":
			transport.DialContext = socks4DialContext(p.Address, p.Username, p.Scheme == "socks4a")
		default:
			proxyURL := fmt.Sprintf("%s://%s", p.Scheme, p.Address)
			if p.Username != "" {
				proxyURL = fmt.Sprintf("%s://%s:%s@%s", p.Scheme, p.Username, p.Password, p.Address)
			}
			if u, err := url.Parse(proxyURL); err == nil {
				transport.Proxy = http.ProxyURL(u)
			}
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		Jar:       solverClient.Jar,
	}
}

func socks4DialContext(proxyAddr, userID string, remoteDNS bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, err
		}

		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			conn.Close()
			return nil, err
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			conn.Close()
			return nil, err
		}

		req := []byte{0x04, 0x01, byte(port >> 8), byte(port)}

		ip := net.ParseIP(host)
		use4a := remoteDNS || ip == nil
		if use4a {
			req = append(req, 0x00, 0x00, 0x00, 0x01)
		} else {
			ip4 := ip.To4()
			if ip4 == nil {
				conn.Close()
				return nil, fmt.Errorf("socks4: not an IPv4 address: %s", host)
			}
			req = append(req, ip4...)
		}

		req = append(req, []byte(userID)...)
		req = append(req, 0x00)
		if use4a {
			req = append(req, []byte(host)...)
			req = append(req, 0x00)
		}

		deadline := time.Now().Add(10 * time.Second)
		if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
			deadline = dl
		}
		conn.SetDeadline(deadline)

		if _, err := conn.Write(req); err != nil {
			conn.Close()
			return nil, err
		}

		var resp [8]byte
		if _, err := io.ReadFull(conn, resp[:]); err != nil {
			conn.Close()
			return nil, err
		}
		if resp[1] != 0x5A {
			conn.Close()
			return nil, fmt.Errorf("socks4: connect rejected (code 0x%02x)", resp[1])
		}

		conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

func initCache() {
	taskCache = cache.New(CacheTTL, CleanupInterval)
}

func initRateLimiter() {
	limiter = rate.NewLimiter(rate.Limit(cfg.RateLimit), cfg.BurstLimit)
}

func initWorkers() {
	workerSem = make(chan struct{}, cfg.MaxWorkers)
}

func setupRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestLogger())
	r.Use(rateLimitMiddleware())

	r.GET("/health", healthHandler)
	if cfg.EnableMetrics {
		r.GET("/metrics", metricsHandler)
	}
	r.POST("/solve", solveHandler)
	r.GET("/solve/:task_id", taskStatusHandler)
	r.POST("/batch", batchSolveHandler)
	r.GET("/proxies", proxiesHandler)
	r.POST("/proxies/reload", reloadProxiesHandler)

	return r
}

func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)
		logDebug("%s %s %d %v", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), latency)
	}
}

func rateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !limiter.Allow() {
			c.JSON(http.StatusTooManyRequests, APIResponse{
				Success: false,
				Error:   "rate limited",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

func healthHandler(c *gin.Context) {
	total, alive := pool.Stats()
	c.JSON(http.StatusOK, APIResponse{
		Success: true,
		Data: gin.H{
			"status":        "healthy",
			"version":       Version,
			"uptime":        time.Since(metrics.StartTime).String(),
			"proxies_total": total,
			"proxies_alive": alive,
			"workers":       cfg.MaxWorkers,
		},
	})
}

func metricsHandler(c *gin.Context) {
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()

	total, alive := pool.Stats()
	c.JSON(http.StatusOK, APIResponse{
		Success: true,
		Data: gin.H{
			"total_requests":      metrics.TotalRequests,
			"successful_solves":   metrics.SuccessfulSolves,
			"failed_solves":       metrics.FailedSolves,
			"success_rate":        float64(metrics.SuccessfulSolves) / float64(max(metrics.TotalRequests, 1)) * 100,
			"active_workers":      metrics.ActiveWorkers,
			"proxies_total":       total,
			"proxies_alive":       alive,
			"avg_latency_ms":      metrics.AvgLatencyMs,
			"requests_per_second": metrics.RequestsPerSecond,
			"uptime_seconds":      time.Since(metrics.StartTime).Seconds(),
		},
	})
}

func solveHandler(c *gin.Context) {
	var req SolveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, APIResponse{Success: false, Error: err.Error()})
		return
	}

	if err := resolveTarget(&req); err != nil {
		c.JSON(http.StatusBadRequest, APIResponse{Success: false, Error: err.Error()})
		return
	}

	taskID := uuid.New().String()
	task := &TurnstileTask{
		ID:           taskID,
		SiteKey:      req.SiteKey,
		PageURL:      req.PageURL,
		Action:       req.Action,
		CData:        req.CData,
		CreatedAt:    time.Now(),
		Status:       "pending",
		Fetch:        req.Fetch,
		FetchMethod:  req.FetchMethod,
		FetchBody:    req.FetchBody,
		FetchHeaders: req.FetchHeaders,
	}

	if req.Proxy != "" {
		task.Proxy = parseProxy(req.Proxy)
	}

	taskCache.Set(taskID, task, CacheTTL)
	atomic.AddUint64(&metrics.TotalRequests, 1)

	if req.Async {
		go processTask(task)
		c.JSON(http.StatusAccepted, APIResponse{
			Success: true,
			Data: SolveResponse{
				TaskID: taskID,
				Status: "queued",
			},
		})
		return
	}

	result := processTask(task)
	c.JSON(http.StatusOK, APIResponse{
		Success: result.Error == nil,
		Data: SolveResponse{
			TaskID:      task.ID,
			Token:       result.Token,
			Status:      task.Status,
			SolverUsed:  result.SolverUsed,
			LatencyMs:   result.Latency.Milliseconds(),
			ProxyUsed:   result.ProxyUsed,
			PageContent: result.PageContent,
			ContentType: result.ContentType,
			StatusCode:  result.StatusCode,
			Headers:     result.Headers,
		},
		Error: errorToString(result.Error),
	})
}

func taskStatusHandler(c *gin.Context) {
	taskID := c.Param("task_id")
	val, ok := taskCache.Get(taskID)
	if !ok {
		c.JSON(http.StatusNotFound, APIResponse{Success: false, Error: "task not found"})
		return
	}
	task := val.(*TurnstileTask)
	c.JSON(http.StatusOK, APIResponse{
		Success: true,
		Data: SolveResponse{
			TaskID:      task.ID,
			Token:       task.Result,
			Status:      task.Status,
			SolverUsed:  task.SolverUsed,
			PageContent: task.PageContent,
			ContentType: task.ContentType,
			StatusCode:  task.StatusCode,
			Headers:     task.Headers,
		},
		Error: errorToString(task.Error),
	})
}

func batchSolveHandler(c *gin.Context) {
	var reqs []SolveRequest
	if err := c.ShouldBindJSON(&reqs); err != nil {
		c.JSON(http.StatusBadRequest, APIResponse{Success: false, Error: err.Error()})
		return
	}

	if len(reqs) > 100 {
		c.JSON(http.StatusBadRequest, APIResponse{Success: false, Error: "batch size max 100"})
		return
	}

	for i := range reqs {
		if err := resolveTarget(&reqs[i]); err != nil {
			c.JSON(http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("request %d: %v", i, err),
			})
			return
		}
	}

	results := make([]SolveResponse, len(reqs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 50)

	for i, req := range reqs {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, r SolveRequest) {
			defer wg.Done()
			defer func() { <-sem }()

			task := &TurnstileTask{
				ID:           uuid.New().String(),
				SiteKey:      r.SiteKey,
				PageURL:      r.PageURL,
				Action:       r.Action,
				CData:        r.CData,
				CreatedAt:    time.Now(),
				Status:       "pending",
				Fetch:        r.Fetch,
				FetchMethod:  r.FetchMethod,
				FetchBody:    r.FetchBody,
				FetchHeaders: r.FetchHeaders,
			}
			if r.Proxy != "" {
				task.Proxy = parseProxy(r.Proxy)
			}

			result := processTask(task)
			results[idx] = SolveResponse{
				TaskID:      task.ID,
				Token:       result.Token,
				Status:      task.Status,
				SolverUsed:  result.SolverUsed,
				LatencyMs:   result.Latency.Milliseconds(),
				ProxyUsed:   result.ProxyUsed,
				PageContent: result.PageContent,
				ContentType: result.ContentType,
				StatusCode:  result.StatusCode,
				Headers:     result.Headers,
			}
		}(i, req)
	}
	wg.Wait()

	c.JSON(http.StatusOK, APIResponse{Success: true, Data: results})
}

func proxiesHandler(c *gin.Context) {
	limit := 200
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	aliveOnly := c.Query("alive") == "true" || c.Query("alive") == "1"

	pool.mu.RLock()
	snapshot := make([]*Proxy, len(pool.proxies))
	copy(snapshot, pool.proxies)
	total, alive := len(pool.proxies), len(pool.alive)
	pool.mu.RUnlock()

	data := make([]gin.H, 0, min(limit, len(snapshot)))
	for _, p := range snapshot {
		if len(data) >= limit {
			break
		}
		p.mu.RLock()
		aliveP := p.Alive && p.Failures < 5
		if aliveOnly && !aliveP {
			p.mu.RUnlock()
			continue
		}
		entry := gin.H{
			"address":   p.Address,
			"scheme":    p.Scheme,
			"alive":     aliveP,
			"latency":   p.Latency.Milliseconds(),
			"failures":  p.Failures,
			"last_used": p.LastUsed,
		}
		p.mu.RUnlock()
		data = append(data, entry)
	}

	c.JSON(http.StatusOK, APIResponse{Success: true, Data: data, Meta: gin.H{
		"total": total,
		"alive": alive,
		"shown": len(data),
		"limit": limit,
	}})
}

func reloadProxiesHandler(c *gin.Context) {
	loadProxies(cfg.ProxiesFile)
	c.JSON(http.StatusOK, APIResponse{Success: true, Data: gin.H{"reloaded": len(pool.proxies)}})
}

func processTask(task *TurnstileTask) SolverResult {
	workerSem <- struct{}{}
	defer func() { <-workerSem }()

	atomic.AddInt32(&metrics.ActiveWorkers, 1)
	defer atomic.AddInt32(&metrics.ActiveWorkers, -1)

	task.Status = "processing"
	task.Attempts++

	var result SolverResult
	var err error
	var proxy *Proxy

	for attempt := 0; attempt < MaxRetries; attempt++ {
		task.Attempts = attempt + 1

		proxy = task.Proxy
		if proxy == nil {
			proxy = pool.Get()
		}

		if proxy == nil && len(pool.proxies) > 0 {
			result = SolverResult{Error: ErrNoProxies}
			break
		}

		start := time.Now()
		token, solverUsed, solveErr := solveTurnstile(task, proxy)
		latency := time.Since(start)

		if solveErr == nil && token != "" {
			result = SolverResult{
				Token:      token,
				SolverUsed: solverUsed,
				Latency:    latency,
				ProxyUsed:  proxyAddress(proxy),
			}
			if proxy != nil {
				pool.MarkAlive(proxy, latency)
			}
			updateMetrics(true, latency)
			break
		}

		if proxy != nil {
			pool.MarkDead(proxy)
		}

		if isRateLimitError(solveErr) {
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}

		err = solveErr
		logDebug("Attempt %d failed: %v", attempt+1, solveErr)
	}

	if result.Token == "" {
		result.Error = err
		if err == nil {
			result.Error = ErrSolvingFailed
		}
		task.Status = "failed"
		task.Error = result.Error
		updateMetrics(false, 0)
	} else {
		task.Status = "completed"
		task.Result = result.Token
		task.SolverUsed = result.SolverUsed

		if task.Fetch {
			fetchStart := time.Now()
			content, contentType, statusCode, headers, fetchErr := fetchPageContent(
				task.PageURL,
				result.Token,
				task.FetchMethod,
				task.FetchBody,
				task.FetchHeaders,
				proxy,
			)
			fetchLatency := time.Since(fetchStart)

			result.PageContent = content
			result.ContentType = contentType
			result.StatusCode = statusCode
			result.Headers = headers
			result.Latency += fetchLatency

			task.PageContent = content
			task.ContentType = contentType
			task.StatusCode = statusCode
			task.Headers = headers

			if fetchErr != nil {
				logDebug("Fetch failed: %v", fetchErr)
			}
		}
	}

	return result
}

func solveTurnstile(task *TurnstileTask, proxy *Proxy) (string, string, error) {
	if cfg.TwoCaptchaKey != "" {
		return solveTwoCaptcha(task, proxy)
	}
	if cfg.CapMonsterKey != "" {
		return solveCapMonster(task, proxy)
	}
	if cfg.CustomSolverURL != "" {
		return solveCustom(task, proxy)
	}
	return solveDirect(task, proxy)
}

func solveTwoCaptcha(task *TurnstileTask, proxy *Proxy) (string, string, error) {
	client := buildHTTPClient(proxy, 60*time.Second)

	createURL := "https://2captcha.com/in.php"
	params := url.Values{
		"key":     {cfg.TwoCaptchaKey},
		"method":  {"turnstile"},
		"sitekey": {task.SiteKey},
		"pageurl": {task.PageURL},
		"json":    {"1"},
		"soft_id": {"4921"},
	}
	if task.Action != "" {
		params.Set("action", task.Action)
	}
	if task.CData != "" {
		params.Set("data", task.CData)
	}
	if proxy != nil {
		params.Set("proxy", proxy.Address)
		if proxy.Username != "" {
			params.Set("proxytype", proxy.Scheme)
			params.Set("proxyuser", proxy.Username)
			params.Set("proxypass", proxy.Password)
		}
	}

	resp, err := client.PostForm(createURL, params)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var createResp struct {
		Status  int    `json:"status"`
		Request string `json:"request"`
		Error   string `json:"error_text"`
	}
	json.Unmarshal(body, &createResp)

	if createResp.Status != 1 {
		return "", "", fmt.Errorf("2captcha create: %s", createResp.Error)
	}

	taskID := createResp.Request
	resultURL := "https://2captcha.com/res.php"

	for i := 0; i < 60; i++ {
		time.Sleep(5 * time.Second)
		resp, err := client.Get(resultURL + "?key=" + cfg.TwoCaptchaKey + "&action=get&id=" + taskID + "&json=1")
		if err != nil {
			continue
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()

		var resultResp struct {
			Status  int    `json:"status"`
			Request string `json:"request"`
			Error   string `json:"error_text"`
		}
		json.Unmarshal(body, &resultResp)

		if resultResp.Status == 1 {
			return resultResp.Request, "2captcha", nil
		}
		if resultResp.Error != "CAPCHA_NOT_READY" {
			return "", "", fmt.Errorf("2captcha: %s", resultResp.Error)
		}
	}
	return "", "", errors.New("2captcha timeout")
}

func solveCapMonster(task *TurnstileTask, proxy *Proxy) (string, string, error) {
	client := buildHTTPClient(proxy, 60*time.Second)

	createPayload := map[string]interface{}{
		"clientKey": cfg.CapMonsterKey,
		"task": map[string]interface{}{
			"type":       "TurnstileTaskProxyless",
			"websiteURL": task.PageURL,
			"websiteKey": task.SiteKey,
			"pageAction": task.Action,
			"data":       task.CData,
			"userAgent":  UserAgent,
		},
	}

	if proxy != nil {
		createPayload["task"] = map[string]interface{}{
			"type":          "TurnstileTask",
			"websiteURL":    task.PageURL,
			"websiteKey":    task.SiteKey,
			"proxyType":     proxy.Scheme,
			"proxyAddress":  strings.Split(proxy.Address, ":")[0],
			"proxyPort":     strings.Split(proxy.Address, ":")[1],
			"proxyLogin":    proxy.Username,
			"proxyPassword": proxy.Password,
			"pageAction":    task.Action,
			"data":          task.CData,
			"userAgent":     UserAgent,
		}
	}

	body, _ := json.Marshal(createPayload)
	resp, err := client.Post("https://api.capmonster.cloud/createTask", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var createResp struct {
		ErrorId int    `json:"errorId"`
		TaskID  string `json:"taskId"`
		Error   string `json:"errorDescription"`
	}
	json.Unmarshal(respBody, &createResp)

	if createResp.ErrorId != 0 {
		return "", "", fmt.Errorf("capmonster create: %s", createResp.Error)
	}

	getPayload := map[string]string{"clientKey": cfg.CapMonsterKey, "taskId": createResp.TaskID}
	for i := 0; i < 60; i++ {
		time.Sleep(3 * time.Second)
		body, _ = json.Marshal(getPayload)
		resp, err = client.Post("https://api.capmonster.cloud/getTaskResult", "application/json", strings.NewReader(string(body)))
		if err != nil {
			continue
		}
		respBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()

		var resultResp struct {
			ErrorId  int               `json:"errorId"`
			Status   string            `json:"status"`
			Solution map[string]string `json:"solution"`
			Error    string            `json:"errorDescription"`
		}
		json.Unmarshal(respBody, &resultResp)

		if resultResp.ErrorId != 0 {
			return "", "", fmt.Errorf("capmonster: %s", resultResp.Error)
		}
		if resultResp.Status == "ready" {
			return resultResp.Solution["token"], "capmonster", nil
		}
	}
	return "", "", errors.New("capmonster timeout")
}

func solveCustom(task *TurnstileTask, proxy *Proxy) (string, string, error) {
	client := buildHTTPClient(proxy, 60*time.Second)

	payload := map[string]interface{}{
		"sitekey": task.SiteKey,
		"pageurl": task.PageURL,
		"action":  task.Action,
		"cdata":   task.CData,
		"proxy":   proxyAddress(proxy),
	}

	body, _ := json.Marshal(payload)
	resp, err := client.Post(cfg.CustomSolverURL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	json.Unmarshal(respBody, &result)

	if result.Error != "" {
		return "", "", errors.New(result.Error)
	}
	return result.Token, "custom", nil
}

func solveDirect(task *TurnstileTask, proxy *Proxy) (string, string, error) {
	client := buildHTTPClient(proxy, RequestTimeout)

	resp, err := client.Get(task.PageURL)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	pageHTML := string(body)

	turnstileParams := extractTurnstileParams(pageHTML)
	if turnstileParams == nil {
		return "", "", errors.New("turnstile params not found in page")
	}

	siteKey := turnstileParams["sitekey"]
	if siteKey == "" {
		siteKey = task.SiteKey
	}

	action := turnstileParams["action"]
	if action == "" {
		action = task.Action
	}

	cData := turnstileParams["cdata"]
	if cData == "" {
		cData = task.CData
	}

	verifyPayload := url.Values{
		"secret":   {""},
		"response": {""},
		"remoteip": {""},
	}

	verifyPayload.Set("secret", "")
	verifyPayload.Set("response", "")
	verifyPayload.Set("remoteip", "")

	verifyReq, _ := http.NewRequest("POST", TurnstileVerify, strings.NewReader(verifyPayload.Encode()))
	verifyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	verifyReq.Header.Set("User-Agent", UserAgent)

	verifyResp, err := client.Do(verifyReq)
	if err != nil {
		return "", "", err
	}
	defer verifyResp.Body.Close()

	verifyBody, _ := io.ReadAll(verifyResp.Body)
	var verifyResult struct {
		Success     bool     `json:"success"`
		ChallengeTS string   `json:"challenge_ts"`
		Hostname    string   `json:"hostname"`
		ErrorCodes  []string `json:"error-codes"`
	}
	json.Unmarshal(verifyBody, &verifyResult)

	if !verifyResult.Success {
		return "", "", fmt.Errorf("turnstile verify failed: %v", verifyResult.ErrorCodes)
	}

	return "", "direct", nil
}

func fetchPageContent(pageURL, token, method, body string, headers map[string]string, proxy *Proxy) (string, string, int, map[string]string, error) {
	client := buildHTTPClient(proxy, RequestTimeout)

	m := strings.ToUpper(method)
	if m == "" {
		m = "POST"
	}

	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	} else if m == "POST" {
		form := url.Values{}
		form.Set("cf-turnstile-response", token)
		reqBody = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequest(m, pageURL, reqBody)
	if err != nil {
		return "", "", 0, nil, err
	}

	if m == "POST" && body == "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Origin", getOrigin(pageURL))
	req.Header.Set("Referer", pageURL)

	if headers != nil {
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	contentType := resp.Header.Get("Content-Type")

	respHeaders := make(map[string]string)
	for k, v := range resp.Header {
		if len(v) > 0 {
			respHeaders[k] = v[0]
		}
	}

	return string(respBody), contentType, resp.StatusCode, respHeaders, nil
}

func getOrigin(pageURL string) string {
	u, _ := url.Parse(pageURL)
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}

func extractTurnstileParams(html string) map[string]string {
	params := make(map[string]string)
	patterns := []string{
		`data-sitekey=["']([^"']+)["']`,
		`data-action=["']([^"']+)["']`,
		`data-cdata=["']([^"']+)["']`,
		`turnstile\.render\([^)]+sitekey["']?\s*:\s*["']([^"']+)["']`,
		`sitekey["']?\s*:\s*["']([^"']+)["']`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(html)
		if len(matches) > 1 {
			if strings.Contains(pattern, "sitekey") {
				params["sitekey"] = matches[1]
			} else if strings.Contains(pattern, "action") {
				params["action"] = matches[1]
			} else if strings.Contains(pattern, "cdata") {
				params["cdata"] = matches[1]
			}
		}
	}
	return params
}

func proxyAddress(p *Proxy) string {
	if p == nil {
		return "direct"
	}
	addr := p.Address
	if p.Username != "" {
		return fmt.Sprintf("%s://%s:%s@%s", p.Scheme, p.Username, p.Password, addr)
	}
	return fmt.Sprintf("%s://%s", p.Scheme, addr)
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "rate limit") || strings.Contains(msg, "429") || strings.Contains(msg, "too many requests")
}

func updateMetrics(success bool, latency time.Duration) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	if success {
		metrics.SuccessfulSolves++
		if latency > 0 {
			metrics.AvgLatencyMs = (metrics.AvgLatencyMs + latency.Milliseconds()) / 2
		}
	} else {
		metrics.FailedSolves++
	}
}

func metricsCollector() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	var lastTotal uint64
	var lastTime = time.Now()

	for range ticker.C {
		metrics.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(lastTime).Seconds()
		metrics.RequestsPerSecond = float64(metrics.TotalRequests-lastTotal) / elapsed
		lastTotal = metrics.TotalRequests
		lastTime = now
		metrics.mu.Unlock()
	}
}

func waitForShutdown(srv *http.Server) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logInfo("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logFatal("Server forced to shutdown: %v", err)
	}
	logInfo("Server exited")
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var i int
		fmt.Sscanf(v, "%d", &i)
		return i
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		return v == "true" || v == "1"
	}
	return def
}

func errorToString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func max(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func logInfo(format string, args ...interface{}) {
	fmt.Printf("[INFO] "+format+"\n", args...)
}

func logWarn(format string, args ...interface{}) {
	fmt.Printf("[WARN] "+format+"\n", args...)
}

func logDebug(format string, args ...interface{}) {
	if os.Getenv("DEBUG") == "true" {
		fmt.Printf("[DEBUG] "+format+"\n", args...)
	}
}

func logFatal(format string, args ...interface{}) {
	fmt.Printf("[FATAL] "+format+"\n", args...)
	os.Exit(1)
}
