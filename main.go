package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"run.pmh.codes/run/auth"
	"run.pmh.codes/run/config"
	k8sclient "run.pmh.codes/run/k8s"
	"run.pmh.codes/run/session"
	"run.pmh.codes/run/terminal"
)

func main() {
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	sess := session.New(cfg.SessionSecret)

	authHandler, err := auth.New(ctx, cfg, sess)
	if err != nil {
		log.Fatalf("oidc init error: %v", err)
	}

	k8sClient, restCfg, err := k8sclient.NewClient(cfg.Kubeconfig)
	if err != nil {
		log.Fatalf("k8s client error: %v", err)
	}

	podManager := k8sclient.NewPodManager(k8sClient, cfg.PodNamespace, cfg.PodImage, cfg.PodCPULimit, cfg.PodMemoryLimit, cfg.PodStorageSize)
	usageHandler, err := k8sclient.NewUsageHandler(restCfg, podManager, cfg.PodNamespace)
	if err != nil {
		log.Fatalf("usage handler error: %v", err)
	}
	termHandler := terminal.New(k8sClient, restCfg, podManager, usageHandler, cfg.PodNamespace, cfg.BaseURL)

	// requireAdmin wraps a handler to only allow authenticated admin users.
	requireAdmin := func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sessData, err := sess.Get(r)
			if err != nil || !session.GetBool(sessData, session.KeyIsAdmin) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		}))
	}

	type userOverview struct {
		UserSub       string  `json:"user_sub"`
		Username      string  `json:"username"`
		PodName       string  `json:"pod_name"`
		PodPhase      string  `json:"pod_phase"`
		PodAgeSeconds int64   `json:"pod_age_seconds"`
		CPUPercent    float64 `json:"cpu_percent"`
		MemoryPercent float64 `json:"memory_percent"`
		Connected     bool    `json:"connected"`
	}

	mux := http.NewServeMux()

	// Service worker — must be served from root, not /static/, to scope the whole app
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Service-Worker-Allowed", "/")
		w.Header().Set("Content-Type", "application/javascript")
		http.ServeFile(w, r, "static/sw.js")
	})

	// Health check (no auth)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Auth routes
	mux.HandleFunc("GET /auth/login", authHandler.Login)
	mux.HandleFunc("GET /auth/callback", authHandler.Callback)
	mux.HandleFunc("GET /auth/logout", authHandler.Logout)

	// Static assets (xterm.js, CSS - no auth needed for assets)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// Restart API (requires auth)
	mux.Handle("POST /api/restart", authHandler.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessData, err := sess.Get(r)
		if err != nil {
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}
		userSub := session.GetString(sessData, session.KeyUserSub)
		termHandler.Restart(w, r, userSub)
	})))

	// Usage API (requires auth)
	mux.Handle("GET /api/usage", authHandler.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessData, err := sess.Get(r)
		if err != nil {
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}
		userSub := session.GetString(sessData, session.KeyUserSub)
		usageHandler.ServeHTTP(w, r, userSub)
	})))

	// Admin UI (requires admin)
	mux.Handle("GET /admin", requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "static/admin.html")
	})))

	// Admin overview API (requires admin)
	mux.Handle("GET /api/admin/overview", requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pods, err := podManager.ListPods(r.Context())
		if err != nil {
			http.Error(w, "failed to list pods", http.StatusInternalServerError)
			return
		}

		allUsage, _ := usageHandler.GetAllUsage(r.Context())
		if allUsage == nil {
			allUsage = make(map[string]*k8sclient.UsageResponse)
		}

		connected := termHandler.ConnectedUsers()

		result := make([]userOverview, 0, len(pods))
		for _, pod := range pods {
			ov := userOverview{
				UserSub:   pod.UserSub,
				Username:  pod.Username,
				PodName:   pod.Name,
				PodPhase:  pod.Phase,
				Connected: connected[pod.UserSub],
			}
			if pod.StartTime != nil {
				ov.PodAgeSeconds = int64(time.Since(pod.StartTime.Time).Seconds())
			}
			if u, ok := allUsage[pod.Name]; ok {
				ov.CPUPercent = u.CPUPercent
				ov.MemoryPercent = u.MemoryPercent
			}
			result = append(result, ov)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})))

	// Current user info API (requires auth)
	mux.Handle("GET /api/me", authHandler.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessData, err := sess.Get(r)
		if err != nil {
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			Username string `json:"username"`
			IsAdmin  bool   `json:"is_admin"`
		}{
			Username: session.GetString(sessData, session.KeyUsername),
			IsAdmin:  session.GetBool(sessData, session.KeyIsAdmin),
		})
	})))

	// Admin: watch a user's session (read-only observer WebSocket)
	mux.Handle("GET /terminal/watch", requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetSub := r.URL.Query().Get("sub")
		if targetSub == "" {
			http.Error(w, "missing sub", http.StatusBadRequest)
			return
		}
		termHandler.ServeWatch(w, r, targetSub)
	})))

	// Admin: root exec session in a pod
	mux.Handle("GET /terminal/root", requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		podName := r.URL.Query().Get("pod")
		if podName == "" {
			http.Error(w, "missing pod", http.StatusBadRequest)
			return
		}
		termHandler.ServeAdminExec(w, r, podName)
	})))

	// Terminal WebSocket (requires auth)
	mux.Handle("GET /terminal", authHandler.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessData, err := sess.Get(r)
		if err != nil {
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}
		userSub := session.GetString(sessData, session.KeyUserSub)
		username := session.GetString(sessData, session.KeyUsername)
		termHandler.ServeHTTP(w, r, userSub, username)
	})))

	// Root - serve terminal UI (requires auth)
	mux.Handle("GET /", authHandler.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "static/index.html")
	})))

	addr := ":" + cfg.Port
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
