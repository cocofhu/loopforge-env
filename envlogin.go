package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// The manager's session cookie stays on its own host, so it cannot sign the
// browser into an environment's subdomain. Instead "open" mints a one-time
// ticket that the environment's subdomain redeems at envLoginPath: the proxy
// signs in to LoopForge as the configured admin and hands the cookie over.
const envLoginPath = "/__lfenv/login"

type loginTicket struct {
	slug    string
	expires time.Time
}

type tickets struct {
	mu sync.Mutex
	m  map[string]loginTicket
}

func (t *tickets) issue(sl string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for k, v := range t.m {
		if now.After(v.expires) {
			delete(t.m, k)
		}
	}
	t.m[id] = loginTicket{slug: sl, expires: now.Add(time.Minute)}
	return id
}

// redeem consumes the ticket; it is valid once, for one environment.
func (t *tickets) redeem(id, sl string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[id]
	delete(t.m, id)
	return ok && v.slug == sl && time.Now().Before(v.expires)
}

// adminLogin reads the account the post script creates, if there is one.
func (s *Server) adminLogin() (login, password string) {
	if s.cfg.PostConfig == "" {
		return "", ""
	}
	b, err := os.ReadFile(s.cfg.PostConfig)
	if err != nil {
		return "", ""
	}
	var c struct {
		Admin struct{ Login, Password string }
	}
	if json.Unmarshal(b, &c) != nil {
		return "", ""
	}
	return c.Admin.Login, c.Admin.Password
}

func (s *Server) handleLoginTicket(w http.ResponseWriter, r *http.Request) {
	sl := r.PathValue("slug")
	if _, ok := s.branchOf(sl); !ok {
		writeErr(w, http.StatusNotFound, "环境不存在")
		return
	}
	login, _ := s.adminLogin()
	if s.cfg.EnvDomain == "" || login == "" {
		// Without a subdomain to redeem on, or an account to sign in as, just open the environment.
		writeJSON(w, map[string]any{"url": ""})
		return
	}
	writeJSON(w, map[string]any{"url": envLoginPath + "?t=" + s.tickets.issue(sl), "login": login})
}

// redeemLogin runs on <slug>.<EnvDomain>. Whatever happens it ends on the
// environment's home page; a failed sign-in just leaves the login form there.
func (s *Server) redeemLogin(w http.ResponseWriter, r *http.Request, sl string, port int) {
	defer http.Redirect(w, r, "/", http.StatusFound)
	if !s.tickets.redeem(r.URL.Query().Get("t"), sl) {
		return
	}
	login, password := s.adminLogin()
	if login == "" {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	body, _ := json.Marshal(map[string]string{"login": login, "password": password})
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/api/auth/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "env login %s: LoopForge answered %s\n", sl, res.Status)
		return
	}
	for _, c := range res.Cookies() {
		if c.Name == "agentflow_user" && c.Value != "" {
			http.SetCookie(w, &http.Cookie{Name: c.Name, Value: c.Value, Path: "/", HttpOnly: true,
				SameSite: http.SameSiteLaxMode, Secure: s.scheme(r) == "https", MaxAge: c.MaxAge})
		}
	}
}

func envPort(root, sl string) int {
	port, _ := strconv.Atoi(readDotEnv(filepath.Join(root, "envs", sl, "src", ".env"))["AGENTFLOW_HTTP_PORT"])
	return port
}
