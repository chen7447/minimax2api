package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"minimax2api/internal/admin"
	"minimax2api/internal/config"
	"minimax2api/internal/gateway"
	"minimax2api/internal/minimax"
	"minimax2api/internal/pool"
	"minimax2api/internal/signin"
	"minimax2api/internal/store"
)

func main() {
	cfg := config.Load()

	st, err := store.Open(cfg.DataDir, cfg.AdminUser, cfg.AdminKey)
	if err != nil {
		log.Fatalf("初始化数据存储失败: %v", err)
	}

	settingsFn := func() config.Settings { return st.Settings() }
	client := minimax.New(settingsFn)
	accountPool := pool.New(st, settingsFn)
	signinSvc := signin.New(st, client, settingsFn, gateway.CredentialOf)
	api := admin.New(st, accountPool, client, signinSvc, settingsFn)
	compat := gateway.New(st, accountPool, client, settingsFn)

	mux := http.NewServeMux()
	api.Register(mux)

	mux.HandleFunc("GET /health", compat.Health)
	mux.HandleFunc("GET /v1/models", compat.Models)
	mux.HandleFunc("POST /v1/chat/completions", compat.ChatCompletions)
	mux.HandleFunc("POST /v1/messages", compat.AnthropicMessages)
	mux.HandleFunc("POST /v1/images/generations", compat.ImageGenerations)
	mux.HandleFunc("POST /v1/videos/generations", compat.VideoGenerations)

	mediaDir := st.Settings().Media.GeneratedDir
	mux.Handle("GET /media/", http.StripPrefix("/media/", http.FileServer(http.Dir(mediaDir))))

	registerDebug(mux)
	registerStatic(mux, cfg.StaticDir)

	handler := withSecurity(withLogging(mux))
	addr := st.Settings().Server.Addr
	if cfg.Addr != "" {
		addr = cfg.Addr
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go janitor(st)

	// The check-in scheduler and credit poller live for as long as the process
	// does, so they take a context cancelled by the same signal that stops the
	// HTTP server.
	runCtx, stopRun := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopRun()
	signinSvc.Start(runCtx)
	// Public-welfare pool: water-level push + credit rotation.
	api.StartGongyi(runCtx)

	go func() {
		log.Printf("MiniMax2API %s 已启动 · 管理台 http://%s · 数据目录 %s", gateway.Version, displayAddr(addr), cfg.DataDir)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常: %v", err)
		}
	}()

	<-runCtx.Done()
	stopRun()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("关闭服务时出错: %v", err)
	}
	if err := st.Save(); err != nil {
		log.Printf("保存数据时出错: %v", err)
	}
	log.Println("已退出")
}

// registerDebug exposes pprof endpoints for diagnosing stalls. They are only
// served to loopback clients so a public deployment cannot leak heap contents.
func registerDebug(mux *http.ServeMux) {
	local := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			host := r.RemoteAddr
			if idx := strings.LastIndex(host, ":"); idx >= 0 {
				host = host[:idx]
			}
			if host != "127.0.0.1" && host != "::1" && host != "localhost" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /debug/pprof/", local(pprof.Index))
	mux.HandleFunc("GET /debug/pprof/goroutine", local(pprof.Handler("goroutine").ServeHTTP))
	mux.HandleFunc("GET /debug/pprof/heap", local(pprof.Handler("heap").ServeHTTP))
	mux.HandleFunc("GET /debug/pprof/mutex", local(pprof.Handler("mutex").ServeHTTP))
	mux.HandleFunc("GET /debug/pprof/block", local(pprof.Handler("block").ServeHTTP))
}

// registerStatic serves the built frontend and falls back to index.html so
// client-side routes work on refresh.
func registerStatic(mux *http.ServeMux, staticDir string) {
	index := filepath.Join(staticDir, "index.html")
	if _, err := os.Stat(index); err != nil {
		log.Printf("提示: 未找到前端产物 %s，仅提供 API（可先执行 cd frontend && npm run build）", staticDir)
		return
	}

	fileServer := http.FileServer(http.Dir(staticDir))
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		// Never let the shell be cached: it names the hashed bundle, so a stale
		// copy keeps pointing at a JS file that no longer exists after a
		// rebuild. Without an explicit header Chrome falls back to heuristic
		// caching based on Last-Modified and can serve a stale index.html.
		w.Header().Set("cache-control", "no-cache")
		http.ServeFile(w, r, index)
	}

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			serveIndex(w, r)
			return
		}
		// API prefixes are registered separately; unknown *file-like* paths must
		// 404 (previously every miss returned index.html, which made /data/app.json
		// and /.env look like 200s to scanners). Only extensionless SPA routes
		// fall back to the shell.
		clean := filepath.Clean(strings.TrimPrefix(path, "/"))
		if _, err := os.Stat(filepath.Join(staticDir, clean)); err != nil {
			if strings.HasPrefix(path, "/assets/") || filepath.Ext(clean) != "" ||
				strings.HasPrefix(path, "/.") || strings.Contains(path, "/.") {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, r)
			return
		}
		// Vite fingerprints asset filenames, so they can be cached forever.
		if strings.HasPrefix(path, "/assets/") {
			w.Header().Set("cache-control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// janitor purges expired audits in the background.
func janitor(st *store.Store) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		st.PurgeAudits()
	}
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	return addr
}

func withSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Behind cloudflared the origin always sees plain HTTP; the client
		// scheme is in X-Forwarded-Proto. Redirect plaintext clients instead of
		// serving keys/cookies over HTTP.
		if r.Header.Get("X-Forwarded-Proto") == "http" {
			target := "https://" + r.Host + r.URL.RequestURI()
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin") || strings.HasPrefix(r.URL.Path, "/v1") || r.URL.Path == "/health" {
			started := time.Now()
			writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(writer, r)
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, writer.status, time.Since(started).Round(time.Millisecond))
			return
		}
		next.ServeHTTP(w, r)
	})
}
