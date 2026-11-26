package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackm/chronohost/internal/config"
)

func TestApplicationFlow(t *testing.T) {
	t.Setenv("CHRONOHOST_ADMIN_USER", "root")
	t.Setenv("CHRONOHOST_ADMIN_PASSWORD", "a-very-secret-password")

	root := t.TempDir()
	app, err := New(root, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	do := func(method, path string, body any) (*http.Response, []byte) {
		t.Helper()
		var r io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			r = bytes.NewReader(encoded)
		}
		req, err := http.NewRequest(method, server.URL+path, r)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res, data
	}

	// Public health check and authentication redirect.
	res, body := do("GET", "/healthz", nil)
	if res.StatusCode != 200 || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("healthz = %d %s", res.StatusCode, body)
	}
	res, _ = do("GET", "/", nil)
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "/login") {
		t.Fatalf("unauthenticated dashboard = %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// Form login.
	loginReq, _ := http.NewRequest("POST", server.URL+"/login", strings.NewReader("username=root&password=a-very-secret-password&next=/"))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRes, err := client.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	loginRes.Body.Close()
	if loginRes.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", loginRes.StatusCode)
	}

	res, body = do("GET", "/instances/new", nil)
	if res.StatusCode != 200 || !strings.Contains(string(body), "Make a Server") {
		t.Fatalf("wizard page = %d (%d bytes)", res.StatusCode, len(body))
	}
	for _, path := range []string{"/", "/versions", "/mods", "/backups", "/settings", "/users"} {
		res, body = do("GET", path, nil)
		if res.StatusCode != 200 {
			t.Fatalf("page %s = %d: %s", path, res.StatusCode, body)
		}
	}

	// Wizard preview produces a complete config.
	form := config.DefaultWizardForm()
	form.ServerName = "Temporal Test"
	form.Port = 42420
	res, body = do("POST", "/api/instances/preview", map[string]any{"form": form})
	if res.StatusCode != 200 {
		t.Fatalf("preview = %d: %s", res.StatusCode, body)
	}
	var preview struct {
		OK      bool                `json:"ok"`
		Errors  []config.FieldError `json:"errors"`
		Preview string              `json:"preview"`
	}
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.OK || len(preview.Errors) != 0 || !strings.Contains(preview.Preview, `"gameMode": "survival"`) {
		t.Fatalf("unexpected preview: ok=%v errors=%+v body=%s", preview.OK, preview.Errors, body)
	}

	// Seed a fake cached binary so provisioning works without network access.
	versionDir := filepath.Join(root, "binaries", "1.2.3")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, "VintagestoryServer.dll"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtimeConfig := `{"runtimeOptions":{"framework":{"name":"Microsoft.NETCore.App","version":"8.0.0"}}}`
	if err := os.WriteFile(filepath.Join(versionDir, "VintagestoryServer.runtimeconfig.json"), []byte(runtimeConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	res, body = do("POST", "/api/instances", map[string]any{
		"form": form, "version": "1.2.3", "start": false, "mods": []any{},
	})
	if res.StatusCode != 201 {
		t.Fatalf("create instance = %d: %s", res.StatusCode, body)
	}
	var created struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		DataPath string `json:"dataPath"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Name != "Temporal Test" {
		t.Fatalf("unexpected instance: %+v", created)
	}

	// serverconfig.json and the default backup schedule are on disk/in DB.
	configPath := filepath.Join(created.DataPath, "serverconfig.json")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	var generated map[string]any
	if err := json.Unmarshal(raw, &generated); err != nil {
		t.Fatal(err)
	}
	wc, ok := generated["WorldConfig"].(map[string]any)
	if !ok || wc["WorldName"] != "Temporal Test" {
		t.Fatalf("unexpected WorldConfig: %+v", generated)
	}
	res, body = do("GET", "/api/instances/"+created.ID+"/schedules", nil)
	if res.StatusCode != 200 {
		t.Fatalf("list schedules = %d: %s", res.StatusCode, body)
	}
	var schedules []struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(body, &schedules); err != nil || len(schedules) != 1 || schedules[0].Kind != "backup" {
		t.Fatalf("unexpected schedules: %s (%v)", body, err)
	}

	// Snapshot, modify the world, and restore it.
	savePath := filepath.Join(created.DataPath, "Saves", "default.vcdbs")
	if err := os.WriteFile(savePath, []byte("world-v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, body = do("POST", "/api/instances/"+created.ID+"/backups", map[string]any{"kind": "manual"})
	if res.StatusCode != 201 {
		t.Fatalf("create backup = %d: %s", res.StatusCode, body)
	}
	var backupRecord struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &backupRecord); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(savePath, []byte("world-v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, body = do("POST", "/api/instances/"+created.ID+"/backups/"+jsonNumber(backupRecord.ID)+"/restore", map[string]any{
		"confirm": true, "restart": false,
	})
	if res.StatusCode != 200 {
		t.Fatalf("restore backup = %d: %s", res.StatusCode, body)
	}
	restored, err := os.ReadFile(savePath)
	if err != nil || string(restored) != "world-v1" {
		t.Fatalf("restore produced %q (%v)", restored, err)
	}

	// RBAC: a viewer cannot control the process.
	res, body = do("POST", "/api/users", map[string]any{
		"username": "watcher", "password": "viewer-password", "role": "viewer",
	})
	if res.StatusCode != 201 {
		t.Fatalf("create viewer = %d: %s", res.StatusCode, body)
	}
	viewerLogin, _ := http.NewRequest("POST", server.URL+"/login", strings.NewReader("username=watcher&password=viewer-password&next=/"))
	viewerLogin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	viewerRes, err := client.Do(viewerLogin)
	if err != nil {
		t.Fatal(err)
	}
	viewerRes.Body.Close()
	res, body = do("POST", "/api/instances/"+created.ID+"/start", map[string]any{})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer start = %d: %s", res.StatusCode, body)
	}
	res, body = do("GET", "/api/instances/"+created.ID+"/logs", nil)
	if res.StatusCode != 200 {
		t.Fatalf("viewer logs = %d: %s", res.StatusCode, body)
	}

	// The console replays status over WebSocket but denies commands to viewers.
	dialer := websocket.Dialer{Jar: jar}
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/console?instance=" + created.ID
	conn, _, err := dialer.Dial(wsURL, http.Header{"Origin": []string{server.URL}})
	if err != nil {
		t.Fatalf("dial console: %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var status consoleMessage
	if err := conn.ReadJSON(&status); err != nil || status.Type != "status" {
		t.Fatalf("console status = %+v (%v)", status, err)
	}
	if err := conn.WriteJSON(map[string]string{"type": "command", "command": "time is 1000"}); err != nil {
		t.Fatal(err)
	}
	var denied consoleMessage
	if err := conn.ReadJSON(&denied); err != nil || denied.Type != "error" || denied.Data != "read-only role" {
		t.Fatalf("viewer console command = %+v (%v)", denied, err)
	}
}

func jsonNumber(n int64) string {
	encoded, _ := json.Marshal(n)
	return string(encoded)
}
