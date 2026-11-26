package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackm/chronohost/internal/store"
)

var consoleUpgrader = websocket.Upgrader{
	ReadBufferSize: 4096, WriteBufferSize: 8192,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		return strings.Contains(origin, r.Host)
	},
}

type consoleMessage struct {
	Type string `json:"type"` // line|status|error|ack
	Line any    `json:"line,omitempty"`
	Data any    `json:"data,omitempty"`
}

func (a *App) wsConsole(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		a.fail(w, 401, errBadRequest("authentication required"))
		return
	}
	id := r.URL.Query().Get("instance")
	if id == "" {
		a.fail(w, 400, errBadRequest("instance is required"))
		return
	}
	if _, err := a.Store.GetInstance(r.Context(), id); err != nil {
		a.fail(w, 404, err)
		return
	}
	conn, err := consoleUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	canCommand := u.Role == store.RoleAdmin || u.Role == store.RoleOperator
	lines, unsubscribe := a.Procs.Subscribe(id)
	defer unsubscribe()

	var writeMu sync.Mutex
	send := func(msg consoleMessage) bool {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(msg) == nil
	}
	for _, line := range a.Procs.Logs(id, 200) {
		if !send(consoleMessage{Type: "line", Line: line}) {
			return
		}
	}
	if !send(consoleMessage{Type: "status", Data: map[string]any{"status": a.Procs.Status(id), "canCommand": canCommand}}) {
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for line := range lines {
			if !send(consoleMessage{Type: "line", Line: line}) {
				return
			}
		}
	}()

	conn.SetReadLimit(8192)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		}
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		switch req.Type {
		case "command":
			if !canCommand {
				send(consoleMessage{Type: "error", Data: "read-only role"})
				continue
			}
			if strings.TrimSpace(req.Command) == "" {
				continue
			}
			if err := a.Procs.Command(id, req.Command); err != nil {
				send(consoleMessage{Type: "error", Data: err.Error()})
			} else {
				send(consoleMessage{Type: "ack", Data: req.Command})
			}
		case "ping":
			send(consoleMessage{Type: "status", Data: map[string]any{"status": a.Procs.Status(id)}})
		}
	}
}
