package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"chains-automation/operator"
)

const sessionCookie = "chains_operator"
const sessionLifetime = 8 * time.Hour

type browserSession struct {
	context.Context
	cancel context.CancelFunc
	role   string
}

type browserSessions struct {
	cookie   string
	mu       sync.Mutex
	sessions map[string]browserSession
}

func (s *browserSessions) lookup(r *http.Request) (browserSession, bool) {
	cookie, err := r.Cookie(s.cookie)
	if err != nil {
		return browserSession{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[cookie.Value]
	return session, ok && session.Err() == nil
}

func webOrigin(r *http.Request, loopbackProxy bool) (secure, allowed bool) {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" ||
		!strings.EqualFold(origin.Host, r.Host) {
		return false, false
	}
	if origin.Scheme == "https" {
		return true, true
	}
	if origin.Scheme != "http" || r.TLS != nil {
		return false, false
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false, false
	}
	addr, err := netip.ParseAddr(remote)
	host := origin.Hostname()
	hostIP, _ := netip.ParseAddr(host)
	return false, err == nil && (loopbackProxy || addr.Unmap().IsLoopback()) &&
		(strings.EqualFold(host, "localhost") || hostIP.Unmap().IsLoopback())
}

func (s *browserSessions) create(role string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, session := range s.sessions {
		if session.Err() != nil {
			delete(s.sessions, key)
		}
	}
	if len(s.sessions) >= 256 {
		return "", false
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", false
	}
	id := hex.EncodeToString(random[:])
	ctx, cancel := context.WithTimeout(context.Background(), sessionLifetime)
	s.sessions[id] = browserSession{ctx, cancel, role}
	return id, true
}

const householdBase = "/household/v1"

type WebOptions struct {
	Operator       bool
	Household      bool
	LoopbackProxy  bool
	HouseholdToken string
}

// WebHTTP adds the enabled browser applications to the authority.
func (e *Engine) WebHTTP(token string, mappings map[string]HTTPSource, assets fs.FS, options WebOptions) http.Handler {
	api := e.HTTP(token, mappings)
	mux := http.NewServeMux()
	if options.Operator {
		sessions := browserSessions{cookie: sessionCookie, sessions: map[string]browserSession{}}
		sessions.routes(mux, operator.Base, token, "", options.LoopbackProxy)
		mux.Handle(operator.Base+"/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if r.Header.Get("Authorization") == "" {
				if session, ok := sessions.lookup(r); ok {
					if r.Method != http.MethodGet && r.Method != http.MethodHead {
						if _, ok := webOrigin(r, options.LoopbackProxy); !ok {
							http.Error(w, "forbidden origin", http.StatusForbidden)
							return
						}
					}
					ctx, cancel := context.WithCancel(r.Context())
					defer cancel()
					stop := context.AfterFunc(session.Context, cancel)
					defer stop()
					r = r.Clone(ctx)
					r.Header.Set("Authorization", "Bearer "+token)
				}
			}
			api.ServeHTTP(w, r)
		}))
		webPages(mux, assets, "ui/index.html", "/ui/{$}", "/ui/state", "/ui/watch", "/ui/send", "/ui/trace", "/ui/rules", "/ui/devices")
		mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
		})
	}
	if options.Household {
		sessions := browserSessions{cookie: "chains_household", sessions: map[string]browserSession{}}
		sessions.routes(mux, householdBase, token, options.HouseholdToken, options.LoopbackProxy)
		e.dashboardRoutes(mux, &sessions, options.LoopbackProxy)
		webPages(mux, assets, "index.html", "/{$}", "/devices", "/dashboards", "/dashboards/{id}", "/dashboard-editor", "/dashboard-editor/{id}")
	}
	mux.Handle("GET /assets/", http.FileServerFS(assets))
	mux.Handle("/", api)
	return mux
}

func webPages(mux *http.ServeMux, assets fs.FS, name string, routes ...string) {
	index, err := fs.ReadFile(assets, name)
	if err != nil {
		panic("web build is missing " + name)
	}
	for _, route := range routes {
		mux.HandleFunc("GET "+route, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(index))
		})
	}
}

func (sessions *browserSessions) routes(mux *http.ServeMux, base, token, householdToken string, loopbackProxy bool) {
	path := base + "/session"
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		session, ok := sessions.lookup(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, 200, map[string]string{"role": session.role})
	})
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		secure, ok := webOrigin(r, loopbackProxy)
		if !ok {
			http.Error(w, "sign in from the console over HTTPS or loopback", http.StatusForbidden)
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		role := ""
		if token != "" && subtle.ConstantTimeCompare([]byte(body.Token), []byte(token)) == 1 {
			role = "operator"
		} else if householdToken != "" && subtle.ConstantTimeCompare([]byte(body.Token), []byte(householdToken)) == 1 {
			role = "household"
		}
		if role == "" {
			http.Error(w, "unauthorized", 401)
			return
		}
		sessions.revoke(r)
		id, ok := sessions.create(role)
		if !ok {
			http.Error(w, "session capacity reached", http.StatusServiceUnavailable)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessions.cookie, Value: id, Path: base, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLifetime.Seconds())})
		writeJSON(w, 200, map[string]string{"role": role})
	})
	mux.HandleFunc("DELETE "+path, func(w http.ResponseWriter, r *http.Request) {
		secure, ok := webOrigin(r, loopbackProxy)
		if !ok {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		sessions.revoke(r)
		w.Header().Set("Cache-Control", "no-store")
		http.SetCookie(w, &http.Cookie{Name: sessions.cookie, Path: base, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		w.WriteHeader(http.StatusNoContent)
	})
}

func (s *browserSessions) revoke(r *http.Request) {
	cookie, err := r.Cookie(s.cookie)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[cookie.Value]; ok {
		session.cancel()
		delete(s.sessions, cookie.Value)
	}
}
