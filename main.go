package main

import (
	"context"
	"log"
	"net/http"

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
	termHandler := terminal.New(k8sClient, restCfg, cfg.PodNamespace, cfg.PodShell, cfg.BaseURL)

	mux := http.NewServeMux()

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

	// Terminal WebSocket (requires auth)
	mux.Handle("GET /terminal", authHandler.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessData, err := sess.Get(r)
		if err != nil {
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}
		userSub := session.GetString(sessData, session.KeyUserSub)
		if userSub == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		pod, err := podManager.EnsurePod(r.Context(), userSub)
		if err != nil {
			log.Printf("ensure pod error for user %s: %v", userSub, err)
			http.Error(w, "failed to start pod: "+err.Error(), http.StatusInternalServerError)
			return
		}

		termHandler.ServeHTTP(w, r, pod)
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
