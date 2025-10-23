package server

import (
	"net/http"
	"sync/atomic"
)

func (server *Server) markUnhealthy() {
	atomic.StoreInt32(&server.unhealthy, 1)
}

func (server *Server) isUnhealthy() bool {
	return atomic.LoadInt32(&server.unhealthy) == 1
}

func (server *Server) wrapUnhealthy(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if server.isUnhealthy() {
			http.Error(w, "session closed", http.StatusInternalServerError)
			return
		}
		handler.ServeHTTP(w, r)
	})
}
