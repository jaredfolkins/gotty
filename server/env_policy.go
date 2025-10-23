package server

import (
	"net/http"
	"strings"
)

const (
	envQueryParam = "ENV"
	envCookieName = "gotty.env"
	envValueDev   = "dev"
	envValueProd  = "prod"
)

type sessionError string

func (e sessionError) Error() string { return string(e) }

var (
	errSessionActive   sessionError = "Another session is active"
	errServerDestroyed sessionError = "Server has been destroyed"
)

type sessionGuard struct {
	server *Server
	env    string
}

func (server *Server) resolveEnvFromRequest(w http.ResponseWriter, r *http.Request) string {
	envValue := strings.TrimSpace(r.URL.Query().Get(envQueryParam))
	if envValue != "" {
		http.SetCookie(w, &http.Cookie{
			Name:  envCookieName,
			Value: envValue,
			Path:  "/",
		})
	} else if cookie, err := r.Cookie(envCookieName); err == nil {
		envValue = cookie.Value
	}

	if envValue == "" {
		return envValueProd
	}

	return strings.ToLower(envValue)
}

func (server *Server) beginManagedSession(env string) (*sessionGuard, error) {
	server.sessionMu.Lock()
	defer server.sessionMu.Unlock()

	if server.decommissioned {
		return nil, errServerDestroyed
	}

	if env == envValueDev {
		return &sessionGuard{server: server, env: env}, nil
	}

	if server.activeSession {
		return nil, errSessionActive
	}

	server.activeSession = true
	return &sessionGuard{server: server, env: env}, nil
}

func (guard *sessionGuard) finish(decommission bool) bool {
	if guard.env == envValueDev {
		return false
	}

	guard.server.sessionMu.Lock()
	defer guard.server.sessionMu.Unlock()

	guard.server.activeSession = false
	if decommission && !guard.server.decommissioned {
		guard.server.decommissioned = true
		guard.server.markUnhealthy()
		return true
	}

	return false
}

func (server *Server) shouldServeHTTP(_ string) bool {
	server.sessionMu.Lock()
	defer server.sessionMu.Unlock()

	return !server.decommissioned
}
