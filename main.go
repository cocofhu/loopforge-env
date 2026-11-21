package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type Config struct {
	Addr        string
	Token       string
	Root        string
	Script      string
	Repo        string
	Concurrency int
	PublicHost  string
	EnvDomain   string
	Host        string
	PostScript  string
	PostConfig  string
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func loadConfig() (Config, error) {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	if wd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(wd, "lfenv.sh")); err == nil {
			dir = wd
		}
	}
	cfg := Config{
		Addr:       env("LFENV_ADDR", ":8088"),
		Token:      os.Getenv("LFENV_TOKEN"),
		Root:       env("LFENV_ROOT", filepath.Join(dir, ".lfenv")),
		Script:     env("LFENV_SCRIPT", filepath.Join(dir, "lfenv.sh")),
		Repo:       env("LFENV_REPO", "https://github.com/tencent/LoopForge"),
		PublicHost: os.Getenv("LFENV_PUBLIC_HOST"),
		EnvDomain:  strings.ToLower(strings.Trim(os.Getenv("LFENV_ENV_DOMAIN"), ". ")),
		Host:       strings.ToLower(strings.Trim(os.Getenv("LFENV_HOST"), ". ")),
		PostScript: os.Getenv("LFENV_POST_SCRIPT"),
		PostConfig: os.Getenv("LFENV_POST_CONFIG"),
	}
	if path := os.Getenv("LFENV_TOKEN_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		cfg.Token = string(b)
	}
	cfg.Token = strings.TrimSpace(cfg.Token)
	if cfg.Token == "" {
		return cfg, errors.New("LFENV_TOKEN (or LFENV_TOKEN_FILE) is required")
	}
	n, err := strconv.Atoi(env("LFENV_BUILD_CONCURRENCY", "2"))
	if err != nil || n < 1 {
		return cfg, errors.New("LFENV_BUILD_CONCURRENCY must be a positive integer")
	}
	cfg.Concurrency = n
	if cfg.PostScript != "" {
		// lfenv.sh reads LFENV_POST_SCRIPT itself (inherited); this only fails fast on a bad path.
		st, err := os.Stat(cfg.PostScript)
		if err != nil {
			return cfg, fmt.Errorf("LFENV_POST_SCRIPT: %w", err)
		}
		if st.Mode()&0o111 == 0 {
			return cfg, fmt.Errorf("LFENV_POST_SCRIPT %s is not executable", cfg.PostScript)
		}
	}
	if cfg.PostConfig != "" {
		cfg.PostConfig, _ = filepath.Abs(cfg.PostConfig)
		// The script reads it from its environment, which builds inherit from this process.
		os.Setenv("LFENV_POST_CONFIG", cfg.PostConfig)
	}
	cfg.Root, _ = filepath.Abs(cfg.Root)
	cfg.Script, _ = filepath.Abs(cfg.Script)
	if _, err := os.Stat(cfg.Script); err != nil {
		return cfg, fmt.Errorf("lfenv.sh not found: %w", err)
	}
	return cfg, os.MkdirAll(cfg.Root, 0o755)
}

type Server struct {
	cfg      Config
	queue    *Queue
	branches *Branches
	docker   *dockerAPI
	pulls    *Pulls
	tickets  tickets
	session  string
}

const sessionCookie = "lfenv_session"

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	q, err := NewQueue(cfg)
	if err != nil {
		log.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(cfg.Token))
	mac.Write([]byte("lfenv-session-v1"))
	s := &Server{cfg: cfg, queue: q, branches: &Branches{repo: cfg.Repo}, docker: newDockerAPI(),
		pulls: newPulls(cfg.Repo, os.Getenv("LFENV_GITHUB_TOKEN")), tickets: tickets{m: map[string]loginTicket{}}, session: hex.EncodeToString(mac.Sum(nil))}

	srv := &http.Server{Addr: cfg.Addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		log.Print("shutting down: stopping running jobs")
		q.Shutdown(20 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("lfenv listening on %s (root %s, %d concurrent builds)", cfg.Addr, cfg.Root, cfg.Concurrency)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (s *Server) routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)
	api.HandleFunc("GET /api/branches", s.handleBranches)
	api.HandleFunc("GET /api/pulls", s.handlePulls)
	api.HandleFunc("GET /api/tags", s.handleTags)
	api.HandleFunc("POST /api/envs", s.handleCreate)
	api.HandleFunc("POST /api/envs/{slug}/rebuild", s.handleRebuild)
	api.HandleFunc("DELETE /api/envs/{slug}", s.handleDestroy)
	api.HandleFunc("GET /api/envs/{slug}/logs", s.handleRuntimeLogs)
	api.HandleFunc("POST /api/envs/{slug}/login-ticket", s.handleLoginTicket)
	api.HandleFunc("GET /api/envs/{slug}/containers", s.handleContainers)
	api.HandleFunc("GET /api/envs/{slug}/containers/{id}/terminal", s.handleTerminal)
	api.HandleFunc("GET /api/post-init", s.handlePostInitGet)
	api.HandleFunc("PUT /api/post-init/{key}", s.handlePostInitPut)
	api.HandleFunc("POST /api/jobs/{id}/cancel", s.handleCancel)
	api.HandleFunc("GET /api/jobs/{id}/log", s.handleJobLog)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tls-allowed", s.handleTLSAllowed)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.Handle("/api/", s.auth(api))
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("/", revalidate(static, http.FileServerFS(static)))
	return s.envProxy(mux)
}

// revalidate makes browsers check embedded files on every load. They carry no
// modification time, so without an ETag a deploy could leave stale assets cached.
func revalidate(files fs.FS, next http.Handler) http.Handler {
	etags := map[string]string{}
	_ = fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, err := fs.ReadFile(files, path); err == nil {
				sum := sha256.Sum256(b)
				etags["/"+path] = `"` + hex.EncodeToString(sum[:8]) + `"`
			}
		}
		return nil
	})
	etags["/"] = etags["/index.html"]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag, ok := etags[r.URL.Path]; ok {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

// envProxy forwards <slug>.<EnvDomain> to that environment's platform port, so
// every environment is reachable through the manager's own port.
func (s *Server) envProxy(next http.Handler) http.Handler {
	if s.cfg.EnvDomain == "" {
		return next
	}
	suffix := "." + s.cfg.EnvDomain
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		sl, ok := strings.CutSuffix(strings.ToLower(host), suffix)
		if !ok || sl == "" || strings.Contains(sl, ".") {
			next.ServeHTTP(w, r)
			return
		}
		port := envPort(s.cfg.Root, sl)
		if port == 0 {
			http.Error(w, "环境 "+sl+" 不存在", http.StatusNotFound)
			return
		}
		if r.URL.Path == envLoginPath {
			s.redeemLogin(w, r, sl, port)
			return
		}
		target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
		proto := s.scheme(r)
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = pr.In.Host
				pr.SetXForwarded()
				pr.Out.Header.Set("X-Forwarded-Proto", proto)
			},
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				http.Error(w, "环境 "+sl+" 未就绪，请在管理平台查看状态", http.StatusBadGateway)
			},
		}
		proxy.ServeHTTP(w, r)
	})
}

// scheme is the scheme the client used. X-Forwarded-Proto is only trusted from
// a reverse proxy on this host, such as the Caddy that terminates TLS.
func (s *Server) scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() && r.Header.Get("X-Forwarded-Proto") == "https" {
			return "https"
		}
	}
	return "http"
}

// handleTLSAllowed answers the on-demand TLS check of the fronting proxy: only
// the manager host and existing environments get certificates.
func (s *Server) handleTLSAllowed(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(strings.TrimSuffix(r.URL.Query().Get("domain"), "."))
	allowed := domain != "" && domain == s.cfg.Host
	if !allowed && s.cfg.EnvDomain != "" {
		if sl, ok := strings.CutSuffix(domain, "."+s.cfg.EnvDomain); ok && !strings.Contains(sl, ".") {
			_, allowed = s.branchOf(sl)
		}
	}
	if !allowed {
		http.Error(w, "unknown host", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) envURL(r *http.Request, e Env) string {
	if s.cfg.EnvDomain != "" {
		host := e.Slug + "." + s.cfg.EnvDomain
		if _, port, err := net.SplitHostPort(r.Host); err == nil {
			host = net.JoinHostPort(host, port)
		}
		return s.scheme(r) + "://" + host
	}
	host := s.cfg.PublicHost
	if host == "" {
		host = r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
	}
	return fmt.Sprintf("http://%s:%d", host, e.Port)
}

func (s *Server) authorized(r *http.Request) bool {
	if c, err := r.Cookie(sessionCookie); err == nil && hmac.Equal([]byte(c.Value), []byte(s.session)) {
		return true
	}
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(t)), []byte(s.cfg.Token)) == 1
	}
	return false
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct{ Token string }
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Token)), []byte(s.cfg.Token)) != 1 {
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "令牌不正确")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: s.session, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.scheme(r) == "https", MaxAge: 30 * 24 * 3600})
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}

type envView struct {
	Env
	PR      *PR    `json:"pr,omitempty"`
	Subject string `json:"subject,omitempty"`
	Job     *Job   `json:"job,omitempty"`
	LastJob *Job   `json:"lastJob,omitempty"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	jobs, stats := s.queue.Snapshot(0)
	views := []*envView{}
	bySlug := map[string]*envView{}
	for _, e := range listEnvs(r.Context(), s.cfg.Root) {
		if e.Port > 0 {
			e.URL = s.envURL(r, e)
		}
		v := &envView{Env: e}
		views = append(views, v)
		bySlug[e.Slug] = v
	}
	// Jobs are newest first: the first active job is the one shown, the first finished one is the last result.
	for i := range jobs {
		j := &jobs[i]
		v := bySlug[j.Slug]
		if v == nil {
			if !j.Active() {
				continue
			}
			v = &envView{Env: Env{Branch: j.Branch, Slug: j.Slug, State: "absent", CreatedAt: j.CreatedAt}}
			views = append([]*envView{v}, views...)
			bySlug[j.Slug] = v
		}
		if j.Active() {
			v.Job = j
		} else if v.LastJob == nil {
			v.LastJob = j
		}
	}
	for _, v := range views {
		if n := prNumber(v.Branch); n > 0 {
			v.PR = s.pulls.Cached(n)
		} else if strings.Contains(v.Branch, ":") {
			v.Subject = s.commitSubject(r.Context(), v.Commit)
		}
	}
	if len(jobs) > 100 {
		jobs = jobs[:100]
	}
	writeJSON(w, map[string]any{"envs": views, "jobs": jobs, "queue": stats, "repo": s.cfg.Repo, "postScript": baseName(s.cfg.PostScript), "postConfig": baseName(s.cfg.PostConfig)})
}

func baseName(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}

func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request) {
	names, err := s.branches.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "无法读取分支列表: "+err.Error())
		return
	}
	writeJSON(w, names)
}

func (s *Server) handlePulls(w http.ResponseWriter, r *http.Request) {
	list, err := s.pulls.Open(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "无法读取 PR 列表: "+err.Error())
		return
	}
	writeJSON(w, list)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct{ Branch string }
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	branch := strings.TrimSpace(body.Branch)
	if n, ok, err := s.pulls.Parse(branch); ok {
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.enqueueBuild(w, r, fmt.Sprintf("pull/%d", n))
		return
	}
	ref, code, err := s.resolveRef(r.Context(), branch)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	s.enqueueBuild(w, r, ref)
}

func (s *Server) handleRebuild(w http.ResponseWriter, r *http.Request) {
	branch, ok := s.branchOf(r.PathValue("slug"))
	if !ok {
		writeErr(w, http.StatusNotFound, "环境不存在")
		return
	}
	s.enqueueBuild(w, r, branch)
}

func (s *Server) enqueueBuild(w http.ResponseWriter, r *http.Request, branch string) {
	sl := slug(branch)
	if sl == "" {
		writeErr(w, http.StatusBadRequest, "分支名不合法")
		return
	}
	if owner, ok := s.branchOf(sl); ok && owner != branch {
		writeErr(w, http.StatusConflict, fmt.Sprintf("环境名 %s 已被分支 %s 占用", sl, owner))
		return
	}
	if n := prNumber(branch); n > 0 {
		if ok, err := s.pulls.Exists(r.Context(), n); err == nil && !ok {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("PR #%d 不存在", n))
			return
		}
		s.pulls.Cached(n)
	} else if strings.Contains(branch, ":") {
		// tag:/commit: refs were checked when they were resolved.
	} else if names, err := s.branches.List(r.Context()); err == nil && !contains(names, branch) {
		writeErr(w, http.StatusNotFound, "远端不存在分支 "+branch)
		return
	}
	writeJSON(w, s.queue.Enqueue(KindBuild, branch, sl))
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	sl := r.PathValue("slug")
	branch, ok := s.branchOf(sl)
	if !ok {
		writeErr(w, http.StatusNotFound, "环境不存在")
		return
	}
	if _, err := os.Stat(filepath.Join(s.cfg.Root, "envs", sl)); os.IsNotExist(err) {
		// Nothing was created yet: dropping the pending build is the whole teardown.
		if !s.queue.CancelSlug(sl) {
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
	}
	writeJSON(w, s.queue.Enqueue(KindDestroy, branch, sl))
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.queue.Cancel(r.PathValue("id")); err != nil {
		code := http.StatusConflict
		if errors.Is(err, ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// branchOf resolves an environment name from disk or from an active job.
func (s *Server) branchOf(sl string) (string, bool) {
	if sl == "" || strings.ContainsAny(sl, "/.") {
		return "", false
	}
	if b := readDotEnv(filepath.Join(s.cfg.Root, "envs", sl, "src", ".env"))["LFENV_BRANCH"]; b != "" {
		return b, true
	}
	return s.queue.BranchOf(sl)
}

// handleJobLog returns the whole log, or follows it as Server-Sent Events when
// the client accepts text/event-stream, ending with an "end" event.
func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.queue.Get(id); !ok {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.ServeFile(w, r, s.queue.LogPath(id))
		return
	}
	sse, ok := newSSE(w)
	if !ok {
		return
	}
	var f *os.File
	for f == nil {
		var err error
		if f, err = os.Open(s.queue.LogPath(id)); err == nil {
			break
		}
		// A queued job has no log yet.
		if job, _ := s.queue.Get(id); !job.Active() {
			sse.send("end", string(job.Status))
			return
		}
		if !sleepCtx(r.Context(), time.Second) {
			return
		}
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	var pending []byte
	for {
		n, _ := f.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			if i := lastNewline(pending); i >= 0 {
				sse.send("log", string(pending[:i+1]))
				pending = append([]byte(nil), pending[i+1:]...)
			}
			continue
		}
		job, _ := s.queue.Get(id)
		if !job.Active() {
			// The process has exited, so the file is complete; drain what is left.
			rest, _ := io.ReadAll(f)
			if tail := string(append(pending, rest...)); tail != "" {
				sse.send("log", tail)
			}
			sse.send("end", string(job.Status))
			return
		}
		if !sleepCtx(r.Context(), 400*time.Millisecond) {
			return
		}
	}
}

func (s *Server) handleRuntimeLogs(w http.ResponseWriter, r *http.Request) {
	src := filepath.Join(s.cfg.Root, "envs", r.PathValue("slug"), "src")
	if _, ok := s.branchOf(r.PathValue("slug")); !ok {
		writeErr(w, http.StatusNotFound, "环境不存在")
		return
	}
	if _, err := os.Stat(filepath.Join(src, ".env")); err != nil {
		writeErr(w, http.StatusNotFound, "环境尚未创建")
		return
	}
	sse, ok := newSSE(w)
	if !ok {
		return
	}
	cmd := exec.CommandContext(r.Context(), "docker", "compose", "--profile", "platform", "logs", "--follow", "--no-color", "--tail", "500", "platform")
	cmd.Dir = src
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		sse.send("end", err.Error())
		return
	}
	go func() { _ = cmd.Wait(); pw.Close() }()
	buf := make([]byte, 32*1024)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			sse.send("log", string(buf[:n]))
		}
		if err != nil {
			sse.send("end", "closed")
			return
		}
	}
}

type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSE(w http.ResponseWriter) (*sseWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseWriter{w, f}, true
}

func (s *sseWriter) send(event, data string) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b)
	s.f.Flush()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func lastNewline(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' {
			return i
		}
	}
	return -1
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
