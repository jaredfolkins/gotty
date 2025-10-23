package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/gorilla/websocket"
	pkgerrors "github.com/pkg/errors"

	"github.com/sorenisanerd/gotty/webtty"
)

func (server *Server) generateHandleWS(ctx context.Context, cancel context.CancelFunc, counter *counter) http.HandlerFunc {
	once := new(int64)

	go func() {
		select {
		case <-counter.timer().C:
			cancel()
		case <-ctx.Done():
		}
	}()

	return func(w http.ResponseWriter, r *http.Request) {
		env := server.resolveEnvFromRequest(w, r)

		if server.options.Once {
			success := atomic.CompareAndSwapInt64(once, 0, 1)
			if !success {
				http.Error(w, "Server is shutting down", http.StatusServiceUnavailable)
				return
			}
		}

		guard, err := server.beginManagedSession(env)
		if err != nil {
			status := http.StatusServiceUnavailable
			message := err.Error()
			if err == errServerDestroyed {
				message = "Server is unavailable"
			}
			http.Error(w, message, status)
			return
		}

		var (
			counterIncremented        bool
			sessionShouldDecommission bool
		)

		closeReason := "unknown reason"

		defer func() {
			if guard != nil {
				destroyed := guard.finish(sessionShouldDecommission)
				if destroyed {
					log.Printf("Server decommissioned after connection from %s", r.RemoteAddr)
				}
			}
		}()

		defer func() {
			if !counterIncremented {
				return
			}
			num := counter.done()
			log.Printf(
				"Connection closed by %s: %s, connections: %d/%d",
				closeReason, r.RemoteAddr, num, server.options.MaxConnection,
			)

			if server.options.Once {
				cancel()
			}
		}()

		if r.Method != "GET" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		num := counter.add(1)
		counterIncremented = true
		log.Printf("New client connected: %s, connections: %d/%d", r.RemoteAddr, num, server.options.MaxConnection)

		conn, err := server.upgrader.Upgrade(w, r, nil)
		if err != nil {
			closeReason = err.Error()
			return
		}
		defer conn.Close()

		// Check if max connections exceeded after upgrade so we can send a proper close message
		if int64(server.options.MaxConnection) != 0 {
			if num > server.options.MaxConnection {
				closeReason = "exceeding max number of connections"
				// Send close frame with custom code 4000 and reason
				conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(4000, "Another session is active"))
				return
			}
		}

		var headers map[string][]string
		if server.options.PassHeaders {
			headers = r.Header
		}

		// Extract query parameters from the HTTP request
		queryParams := r.URL.Query()
		log.Printf("HTTP Query Params: %v", queryParams)

		err = server.processWSConn(ctx, conn, headers, queryParams)

		if env != envValueDev {
			sessionShouldDecommission = shouldDecommission(err)
		}

		switch err {
		case ctx.Err():
			closeReason = "cancelation"
		case webtty.ErrSlaveClosed:
			closeReason = server.factory.Name()
		case webtty.ErrMasterClosed:
			closeReason = "client"
		default:
			closeReason = fmt.Sprintf("an error: %s", err)
		}
	}
}

func (server *Server) processWSConn(ctx context.Context, conn *websocket.Conn, headers map[string][]string, httpQueryParams url.Values) error {
	typ, initLine, err := conn.ReadMessage()
	if err != nil {
		return pkgerrors.Wrapf(err, "failed to authenticate websocket connection")
	}
	if typ != websocket.TextMessage {
		return pkgerrors.New("failed to authenticate websocket connection: invalid message type")
	}

	var init InitMessage
	err = json.Unmarshal(initLine, &init)
	if err != nil {
		return pkgerrors.Wrapf(err, "failed to authenticate websocket connection")
	}
	if init.AuthToken != server.options.Credential {
		return pkgerrors.New("failed to authenticate websocket connection")
	}

	queryPath := "?"
	if server.options.PermitArguments && init.Arguments != "" {
		queryPath = init.Arguments
	}

	query, err := url.Parse(queryPath)
	if err != nil {
		return pkgerrors.Wrapf(err, "failed to parse arguments")
	}
	params := query.Query()

	// Merge HTTP query parameters with WebSocket init arguments
	// HTTP query parameters take precedence
	for key, values := range httpQueryParams {
		params[key] = values
	}
	log.Printf("Final params being passed to factory: %v", params)

	var slave Slave
	slave, err = server.factory.New(params, headers)
	if err != nil {
		return pkgerrors.Wrapf(err, "failed to create backend")
	}
	defer slave.Close()

	titleVars := server.titleVariables(
		[]string{"server", "master", "slave"},
		map[string]map[string]interface{}{
			"server": server.options.TitleVariables,
			"master": map[string]interface{}{
				"remote_addr": conn.RemoteAddr(),
			},
			"slave": slave.WindowTitleVariables(),
		},
	)

	titleBuf := new(bytes.Buffer)
	err = server.titleTemplate.Execute(titleBuf, titleVars)
	if err != nil {
		return pkgerrors.Wrapf(err, "failed to fill window title template")
	}

	opts := []webtty.Option{
		webtty.WithWindowTitle(titleBuf.Bytes()),
	}
	if server.options.PermitWrite {
		opts = append(opts, webtty.WithPermitWrite())
	}
	if server.options.EnableReconnect {
		opts = append(opts, webtty.WithReconnect(server.options.ReconnectTime))
	}
	if server.options.Width > 0 {
		opts = append(opts, webtty.WithFixedColumns(server.options.Width))
	}
	if server.options.Height > 0 {
		opts = append(opts, webtty.WithFixedRows(server.options.Height))
	}
	tty, err := webtty.New(&wsWrapper{conn}, slave, opts...)
	if err != nil {
		return pkgerrors.Wrapf(err, "failed to create webtty")
	}

	err = tty.Run(ctx)

	return err
}

func (server *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	indexVars, err := server.indexVariables(r)
	if err != nil {
		http.Error(w, "Internal Server Error", 500)
		return
	}

	indexBuf := new(bytes.Buffer)
	err = server.indexTemplate.Execute(indexBuf, indexVars)
	if err != nil {
		http.Error(w, "Internal Server Error", 500)
		return
	}

	w.Write(indexBuf.Bytes())
}

func shouldDecommission(err error) bool {
	if err == nil {
		return true
	}

	cause := pkgerrors.Cause(err)
	switch cause {
	case context.Canceled, context.DeadlineExceeded, webtty.ErrMasterClosed, webtty.ErrSlaveClosed:
		return true
	default:
		return false
	}
}

func (server *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	indexVars, err := server.indexVariables(r)
	if err != nil {
		http.Error(w, "Internal Server Error", 500)
		return
	}

	indexBuf := new(bytes.Buffer)
	err = server.manifestTemplate.Execute(indexBuf, indexVars)
	if err != nil {
		http.Error(w, "Internal Server Error", 500)
		return
	}

	w.Write(indexBuf.Bytes())
}

func (server *Server) indexVariables(r *http.Request) (map[string]interface{}, error) {
	titleVars := server.titleVariables(
		[]string{"server", "master"},
		map[string]map[string]interface{}{
			"server": server.options.TitleVariables,
			"master": map[string]interface{}{
				"remote_addr": r.RemoteAddr,
			},
		},
	)

	titleBuf := new(bytes.Buffer)
	err := server.titleTemplate.Execute(titleBuf, titleVars)
	if err != nil {
		return nil, err
	}

	indexVars := map[string]interface{}{
		"title": titleBuf.String(),
	}
	return indexVars, err
}

func (server *Server) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	// @TODO hashing?
	w.Write([]byte("var gotty_auth_token = '" + server.options.Credential + "';"))
}

func (server *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	lines := []string{
		"var gotty_term = 'xterm';",
		"var gotty_ws_query_args = '" + server.options.WSQueryArgs + "';",
	}

	w.Write([]byte(strings.Join(lines, "\n")))
}

// titleVariables merges maps in a specified order.
// varUnits are name-keyed maps, whose names will be iterated using order.
func (server *Server) titleVariables(order []string, varUnits map[string]map[string]interface{}) map[string]interface{} {
	titleVars := map[string]interface{}{}

	for _, name := range order {
		vars, ok := varUnits[name]
		if !ok {
			panic("title variable name error")
		}
		for key, val := range vars {
			titleVars[key] = val
		}
	}

	// safe net for conflicted keys
	for _, name := range order {
		titleVars[name] = varUnits[name]
	}

	return titleVars
}
