// Package web serves Medulla's server-rendered UI.
package web

import (
	"context"
	"embed"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hpoznanski/medulla/internal/auth"
	"github.com/hpoznanski/medulla/internal/config"
	"github.com/hpoznanski/medulla/internal/es"
	"github.com/hpoznanski/medulla/internal/rbac"
)

//go:embed templates/* static/*
var assets embed.FS

const sessionCookie = "medulla_session"

type Server struct {
	cfg       *config.Config
	auth      *auth.Authenticator
	sessions  *auth.Codec
	rbac      *rbac.Store
	clusters  *es.Registry
	logger    *slog.Logger
	loginRate *auth.RateLimiter
	proxies   []*net.IPNet
	tmpl      *template.Template
	mux       *http.ServeMux
}

// clientIP returns the real client address. X-Forwarded-For is trusted only
// when the direct peer is inside a configured trusted_proxies CIDR; the
// rightmost non-trusted hop wins, so clients cannot spoof their way past the
// login rate limit.
func (s *Server) clientIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if len(s.proxies) == 0 || !s.ipTrusted(peer) {
		return peer
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		if !s.ipTrusted(hop) {
			return hop
		}
		peer = hop
	}
	return peer
}

func (s *Server) ipTrusted(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, p := range s.proxies {
		if p.Contains(parsed) {
			return true
		}
	}
	return false
}

func NewServer(
	cfg *config.Config,
	authn *auth.Authenticator,
	sessions *auth.Codec,
	store *rbac.Store,
	clusters *es.Registry,
	logger *slog.Logger,
) (*Server, error) {
	tmpl, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}

	var proxies []*net.IPNet
	for _, cidr := range cfg.TrustedProxies {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, err // config.Load validates; guard for direct construction
		}
		proxies = append(proxies, ipnet)
	}

	s := &Server{
		cfg:       cfg,
		auth:      authn,
		sessions:  sessions,
		rbac:      store,
		clusters:  clusters,
		logger:    logger,
		loginRate: auth.NewRateLimiter(5, 10), // burst 5, 10 attempts/min per IP
		proxies:   proxies,
		tmpl:      tmpl,
		mux:       http.NewServeMux(),
	}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	s.mux.Handle("GET /static/", http.FileServerFS(assets))
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)
	s.mux.Handle("GET /{$}", s.requireSession(s.handleHome))
	s.mux.Handle("GET /c/{cluster}/overview", s.requirePerm(rbac.View, s.handleOverview))
	s.mux.Handle("GET /c/{cluster}/indices", s.requirePerm(rbac.View, s.handleIndices))
	s.mux.Handle("GET /c/{cluster}/indices/{index}", s.requirePerm(rbac.View, s.handleIndexDetail))
	s.mux.Handle("POST /c/{cluster}/indices", s.requirePerm(rbac.IndexWrite, s.handleIndexCreate))
	s.mux.Handle("POST /c/{cluster}/indices/{index}/{action}", s.requirePerm(rbac.IndexWrite, s.handleIndexAction))
	s.mux.Handle("GET /c/{cluster}/cat/{endpoint}", s.requirePerm(rbac.View, s.handleCat))
	s.mux.Handle("GET /c/{cluster}/console", s.requirePerm(rbac.RestGet, s.handleConsolePage))
	s.mux.Handle("POST /c/{cluster}/console", s.requirePerm(rbac.RestGet, s.handleConsoleRun))
	s.mux.Handle("GET /c/{cluster}/analyze", s.requirePerm(rbac.View, s.handleAnalyze))
	s.mux.Handle("POST /c/{cluster}/analyze", s.requirePerm(rbac.View, s.handleAnalyze))
	s.mux.Handle("GET /c/{cluster}/aliases", s.requirePerm(rbac.View, s.handleAliases))
	s.mux.Handle("POST /c/{cluster}/aliases", s.requirePerm(rbac.AliasWrite, s.handleAliasAction))
	s.mux.Handle("GET /c/{cluster}/templates", s.requirePerm(rbac.View, s.handleTemplates))
	s.mux.Handle("POST /c/{cluster}/templates", s.requirePerm(rbac.TemplateWrite, s.handleTemplateSave))
	s.mux.Handle("POST /c/{cluster}/templates/{name}/delete", s.requirePerm(rbac.TemplateWrite, s.handleTemplateDelete))
	s.mux.Handle("GET /c/{cluster}/settings", s.requirePerm(rbac.View, s.handleClusterSettings))
	s.mux.Handle("POST /c/{cluster}/settings", s.requirePerm(rbac.ClusterWrite, s.handleClusterSettingPut))
	s.mux.Handle("GET /c/{cluster}/snapshots", s.requirePerm(rbac.View, s.handleSnapshots))
	s.mux.Handle("POST /c/{cluster}/snapshots/{action}", s.requirePerm(rbac.SnapshotWrite, s.handleSnapshotAction))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'")
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// sameOrigin rejects cross-site non-GET requests (CSRF defense alongside
// the SameSite=Lax cookie). Requests without an Origin header (curl, htmx
// same-origin) pass; browsers always send Origin on cross-site POSTs.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

type ctxKey int

const (
	sessionKey ctxKey = iota
	clientKey
)

func (s *Server) requireSession(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		sess, err := s.sessions.Decode(cookie.Value)
		if err != nil {
			s.clearSession(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	})
}

func (s *Server) requirePerm(perm rbac.Permission, next http.HandlerFunc) http.Handler {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		cluster := r.PathValue("cluster")
		client, err := s.clusters.Get(cluster)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if !s.rbac.Allowed(sess.Roles, cluster, perm) {
			s.audit(r, "denied", 403)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Resolved client rides in the context so handlers can't forget the
		// existence check.
		next(w, r.WithContext(context.WithValue(r.Context(), clientKey, client)))
	})
}

func sessionFrom(r *http.Request) auth.Session {
	sess, _ := r.Context().Value(sessionKey).(auth.Session)
	return sess
}

// clientFrom returns the cluster client resolved by requirePerm. Panics if
// the route was wired without requirePerm — a bug caught by any request.
func clientFrom(r *http.Request) *es.Client {
	client, ok := r.Context().Value(clientKey).(*es.Client)
	if !ok {
		panic("handler reached without requirePerm middleware")
	}
	return client
}

func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.cfg.Session.InsecureCookie, // secure by default; opt out for HTTP dev only
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.Session.TTL / time.Second),
	})
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
}

// audit emits one structured audit record. Never includes request bodies.
func (s *Server) audit(r *http.Request, outcome string, status int) {
	sess := sessionFrom(r)
	ip := s.clientIP(r)
	s.logger.Info("audit",
		"type", "audit",
		"user", sess.User,
		"roles", sess.Roles,
		"method", r.Method,
		"path", r.URL.Path,
		"outcome", outcome,
		"status", status,
		"ip", ip,
	)
}
