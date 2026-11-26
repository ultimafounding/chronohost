// Package store owns the SQLite state database mounted at /data/db.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Roles used by the web portal and API.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// Instance is a managed Vintage Story server.
type Instance struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	Port         int       `json:"port"`
	DataPath     string    `json:"dataPath"`
	Status       string    `json:"status"`
	AutoStart    bool      `json:"autoStart"`
	PlayStyle    string    `json:"playStyle"`
	GameMode     string    `json:"gameMode"`
	ServerConfig string    `json:"serverConfig"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// User is an authenticated portal user.
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Session is a browser session token.
type Session struct {
	Token     string
	UserID    int64
	ExpiresAt time.Time
}

// Backup is metadata for a world snapshot.
type Backup struct {
	ID         int64     `json:"id"`
	InstanceID string    `json:"instanceId"`
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	Kind       string    `json:"kind"`
	CreatedAt  time.Time `json:"createdAt"`
	Files      []string  `json:"files"`
}

// Mod is a mod installed into an instance.
type Mod struct {
	ID           int64     `json:"id"`
	InstanceID   string    `json:"instanceId"`
	ModID        int       `json:"modId"`
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	Filename     string    `json:"filename"`
	GameVersions []string  `json:"gameVersions"`
	Pinned       bool      `json:"pinned"`
	Dependencies []string  `json:"dependencies"`
	InstalledAt  time.Time `json:"installedAt"`
}

// Schedule is a recurring job for an instance.
type Schedule struct {
	ID         int64      `json:"id"`
	InstanceID string     `json:"instanceId"`
	Kind       string     `json:"kind"` // backup|restart|announce
	Expr       string     `json:"expr"`
	Message    string     `json:"message"`
	Enabled    bool       `json:"enabled"`
	LastRun    *time.Time `json:"lastRun"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open creates the database directory if needed and applies the schema.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite writes are serialized; keeps WAL predictable.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS instances (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  version TEXT NOT NULL,
  port INTEGER NOT NULL,
  data_path TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'stopped',
  auto_start INTEGER NOT NULL DEFAULT 0,
  play_style TEXT NOT NULL DEFAULT 'standard',
  game_mode TEXT NOT NULL DEFAULT 'survival',
  server_config TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_instances_port ON instances(port);
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'viewer',
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS backups (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  path TEXT NOT NULL,
  size INTEGER NOT NULL,
  kind TEXT NOT NULL,
  created_at TEXT NOT NULL,
  files TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS mods (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  mod_id INTEGER NOT NULL,
  name TEXT NOT NULL,
  version TEXT NOT NULL,
  filename TEXT NOT NULL,
  game_versions TEXT NOT NULL DEFAULT '[]',
  pinned INTEGER NOT NULL DEFAULT 0,
  dependencies TEXT NOT NULL DEFAULT '[]',
  installed_at TEXT NOT NULL,
  UNIQUE(instance_id, mod_id)
);
CREATE TABLE IF NOT EXISTS schedules (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  expr TEXT NOT NULL,
  message TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  last_run TEXT,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL,
  username TEXT NOT NULL,
  action TEXT NOT NULL,
  detail TEXT NOT NULL
);
`
	_, err := s.db.Exec(schema)
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle for transactions in advanced handlers.
func (s *Store) DB() *sql.DB { return s.db }

func parseTime(v sql.NullString) time.Time {
	if !v.Valid || v.String == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parsePtrTime(v sql.NullString) *time.Time {
	t := parseTime(v)
	if t.IsZero() {
		return nil
	}
	return &t
}

func strTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- Instances ---

func scanInstance(row interface{ Scan(...any) error }) (Instance, error) {
	var in Instance
	var auto, created, updated int
	var status sql.NullString
	err := row.Scan(&in.ID, &in.Name, &in.Version, &in.Port, &in.DataPath, &status,
		&auto, &in.PlayStyle, &in.GameMode, &in.ServerConfig, &created, &updated)
	if err != nil {
		return in, err
	}
	in.AutoStart = auto != 0
	in.Status = status.String
	in.CreatedAt = time.Unix(int64(created), 0)
	in.UpdatedAt = time.Unix(int64(updated), 0)
	return in, nil
}

const instanceCols = `id, name, version, port, data_path, status, auto_start, play_style, game_mode, server_config, created_at, updated_at`

// CreateInstance inserts a new instance.
func (s *Store) CreateInstance(ctx context.Context, in Instance) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO instances (`+instanceCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.ID, in.Name, in.Version, in.Port, in.DataPath, in.Status, boolInt(in.AutoStart),
		in.PlayStyle, in.GameMode, in.ServerConfig, now, now)
	return err
}

// GetInstance returns one instance by ID.
func (s *Store) GetInstance(ctx context.Context, id string) (Instance, error) {
	return scanInstance(s.db.QueryRowContext(ctx, `SELECT `+instanceCols+` FROM instances WHERE id=?`, id))
}

// FindInstanceByName looks up an instance by its display name.
func (s *Store) FindInstanceByName(ctx context.Context, name string) (Instance, error) {
	return scanInstance(s.db.QueryRowContext(ctx, `SELECT `+instanceCols+` FROM instances WHERE name=?`, name))
}

// ListInstances returns all instances ordered by name.
func (s *Store) ListInstances(ctx context.Context) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+instanceCols+` FROM instances ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		in, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// UpdateInstance updates mutable fields of an instance.
func (s *Store) UpdateInstance(ctx context.Context, in Instance) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE instances SET name=?, version=?, port=?, status=?, auto_start=?, play_style=?, game_mode=?, server_config=?, updated_at=? WHERE id=?`,
		in.Name, in.Version, in.Port, in.Status, boolInt(in.AutoStart), in.PlayStyle,
		in.GameMode, in.ServerConfig, time.Now().Unix(), in.ID)
	return err
}

// SetInstanceStatus updates only the status column.
func (s *Store) SetInstanceStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE instances SET status=?, updated_at=? WHERE id=?`, status, time.Now().Unix(), id)
	return err
}

// DeleteInstance removes an instance record.
func (s *Store) DeleteInstance(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM instances WHERE id=?`, id)
	return err
}

// PortInUse reports whether another instance uses the given port.
func (s *Store) PortInUse(ctx context.Context, port int, exceptID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM instances WHERE port=? AND id<>?`, port, exceptID).Scan(&count)
	return count > 0, err
}

// --- Users and sessions ---

const userCols = `id, username, password_hash, role, created_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created sql.NullString
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &created); err != nil {
		return u, err
	}
	u.CreatedAt = parseTime(created)
	return u, nil
}

// CreateUser inserts a user with a bcrypt password hash.
func (s *Store) CreateUser(ctx context.Context, username, hash, role string) (User, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO users (username, password_hash, role, created_at) VALUES (?,?,?,?)`,
		username, hash, role, strTime(time.Now()))
	if err != nil {
		return User{}, err
	}
	id, _ := res.LastInsertId()
	return s.GetUser(ctx, id)
}

// GetUser returns a user by ID.
func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, id))
}

// UserByUsername returns a user by name.
func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username=?`, username))
}

// ListUsers returns all users.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserCount returns the number of registered users.
func (s *Store) UserCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// SetUserRole changes a user's role.
func (s *Store) SetUserRole(ctx context.Context, id int64, role string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET role=? WHERE id=?`, role, id)
	return err
}

// SetUserPassword changes a user's password hash.
func (s *Store) SetUserPassword(ctx context.Context, id int64, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, id)
	return err
}

// DeleteUser removes a user and all their sessions.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	return err
}

// CreateSession stores a session token.
func (s *Store) CreateSession(ctx context.Context, token string, userID int64, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token, user_id, expires_at) VALUES (?,?,?)`,
		token, userID, strTime(expires))
	return err
}

// SessionUser resolves a valid, unexpired session token.
func (s *Store) SessionUser(ctx context.Context, token string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `
SELECT u.id, u.username, u.password_hash, u.role, u.created_at
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token=? AND s.expires_at>?`, token, strTime(time.Now())))
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token=?`, token)
	return err
}

// DeleteExpiredSessions prunes old sessions.
func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, strTime(time.Now()))
	return err
}

// --- Settings ---

// GetSetting returns a setting or the provided default.
func (s *Store) GetSetting(ctx context.Context, key, fallback string) string {
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value); err != nil {
		return fallback
	}
	return value
}

// GetSettingBool parses a boolean setting.
func (s *Store) GetSettingBool(ctx context.Context, key string, fallback bool) bool {
	return s.GetSetting(ctx, key, fmt.Sprintf("%t", fallback)) == "true"
}

// GetSettingInt parses an integer setting.
func (s *Store) GetSettingInt(ctx context.Context, key string, fallback int) int {
	var n int
	if _, err := fmt.Sscanf(s.GetSetting(ctx, key, fmt.Sprintf("%d", fallback)), "%d", &n); err != nil {
		return fallback
	}
	return n
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// AllSettings returns all settings as a map.
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// --- Backups ---

func scanBackup(row interface{ Scan(...any) error }) (Backup, error) {
	var b Backup
	var created sql.NullString
	var files string
	if err := row.Scan(&b.ID, &b.InstanceID, &b.Name, &b.Path, &b.Size, &b.Kind, &created, &files); err != nil {
		return b, err
	}
	b.CreatedAt = parseTime(created)
	_ = json.Unmarshal([]byte(files), &b.Files)
	return b, nil
}

const backupCols = `id, instance_id, name, path, size, kind, created_at, files`

// AddBackup records a snapshot.
func (s *Store) AddBackup(ctx context.Context, b Backup) (Backup, error) {
	files, _ := json.Marshal(b.Files)
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO backups (`+backupCols+`) VALUES (NULL,?,?,?,?,?,?,?)`,
		b.InstanceID, b.Name, b.Path, b.Size, b.Kind, strTime(b.CreatedAt), string(files))
	if err != nil {
		return b, err
	}
	id, _ := res.LastInsertId()
	return s.GetBackup(ctx, id)
}

// GetBackup returns a snapshot by ID.
func (s *Store) GetBackup(ctx context.Context, id int64) (Backup, error) {
	return scanBackup(s.db.QueryRowContext(ctx, `SELECT `+backupCols+` FROM backups WHERE id=?`, id))
}

// ListBackups returns snapshots for an instance, newest first.
func (s *Store) ListBackups(ctx context.Context, instanceID string) ([]Backup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupCols+` FROM backups WHERE instance_id=? ORDER BY created_at DESC`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBackup removes snapshot metadata.
func (s *Store) DeleteBackup(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM backups WHERE id=?`, id)
	return err
}

// --- Mods ---

func scanMod(row interface{ Scan(...any) error }) (Mod, error) {
	var m Mod
	var gv, deps string
	var pinned int
	var installed sql.NullString
	if err := row.Scan(&m.ID, &m.InstanceID, &m.ModID, &m.Name, &m.Version, &m.Filename,
		&gv, &pinned, &deps, &installed); err != nil {
		return m, err
	}
	m.Pinned = pinned != 0
	m.InstalledAt = parseTime(installed)
	_ = json.Unmarshal([]byte(gv), &m.GameVersions)
	_ = json.Unmarshal([]byte(deps), &m.Dependencies)
	return m, nil
}

const modCols = `id, instance_id, mod_id, name, version, filename, game_versions, pinned, dependencies, installed_at`

// AddMod records or replaces an installed mod.
func (s *Store) AddMod(ctx context.Context, m Mod) (Mod, error) {
	gv, _ := json.Marshal(m.GameVersions)
	deps, _ := json.Marshal(m.Dependencies)
	if m.InstalledAt.IsZero() {
		m.InstalledAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO mods (`+modCols+`)
VALUES (NULL,?,?,?,?,?,?,?,?,?)
ON CONFLICT(instance_id, mod_id) DO UPDATE SET
 name=excluded.name, version=excluded.version, filename=excluded.filename,
 game_versions=excluded.game_versions, pinned=excluded.pinned,
 dependencies=excluded.dependencies, installed_at=excluded.installed_at`,
		m.InstanceID, m.ModID, m.Name, m.Version, m.Filename, string(gv),
		boolInt(m.Pinned), string(deps), strTime(m.InstalledAt))
	if err != nil {
		return m, err
	}
	return s.GetMod(ctx, m.InstanceID, m.ModID)
}

// GetMod returns an installed mod.
func (s *Store) GetMod(ctx context.Context, instanceID string, modID int) (Mod, error) {
	return scanMod(s.db.QueryRowContext(ctx, `SELECT `+modCols+` FROM mods WHERE instance_id=? AND mod_id=?`, instanceID, modID))
}

// ListMods returns installed mods for an instance.
func (s *Store) ListMods(ctx context.Context, instanceID string) ([]Mod, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+modCols+` FROM mods WHERE instance_id=? ORDER BY name`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mod
	for rows.Next() {
		m, err := scanMod(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetModPinned updates the version-pin flag.
func (s *Store) SetModPinned(ctx context.Context, id int64, pinned bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE mods SET pinned=? WHERE id=?`, boolInt(pinned), id)
	return err
}

// DeleteMod removes an installed mod record.
func (s *Store) DeleteMod(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mods WHERE id=?`, id)
	return err
}

// --- Schedules ---

func scanSchedule(row interface{ Scan(...any) error }) (Schedule, error) {
	var sc Schedule
	var enabled int
	var last, created sql.NullString
	if err := row.Scan(&sc.ID, &sc.InstanceID, &sc.Kind, &sc.Expr, &sc.Message, &enabled, &last, &created); err != nil {
		return sc, err
	}
	sc.Enabled = enabled != 0
	sc.LastRun = parsePtrTime(last)
	sc.CreatedAt = parseTime(created)
	return sc, nil
}

const scheduleCols = `id, instance_id, kind, expr, message, enabled, last_run, created_at`

// AddSchedule inserts a schedule.
func (s *Store) AddSchedule(ctx context.Context, sc Schedule) (Schedule, error) {
	var last any
	if sc.LastRun != nil {
		last = strTime(*sc.LastRun)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO schedules (`+scheduleCols+`) VALUES (NULL,?,?,?,?,?,?,?)`,
		sc.InstanceID, sc.Kind, sc.Expr, sc.Message, boolInt(sc.Enabled), last, strTime(time.Now()))
	if err != nil {
		return sc, err
	}
	id, _ := res.LastInsertId()
	return s.GetSchedule(ctx, id)
}

// GetSchedule returns a schedule by ID.
func (s *Store) GetSchedule(ctx context.Context, id int64) (Schedule, error) {
	return scanSchedule(s.db.QueryRowContext(ctx, `SELECT `+scheduleCols+` FROM schedules WHERE id=?`, id))
}

// ListSchedules returns schedules for one instance.
func (s *Store) ListSchedules(ctx context.Context, instanceID string) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+scheduleCols+` FROM schedules WHERE instance_id=? ORDER BY id`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ListAllSchedules returns every enabled schedule in the database.
func (s *Store) ListAllSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+scheduleCols+` FROM schedules WHERE enabled=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// UpdateSchedule updates a schedule.
func (s *Store) UpdateSchedule(ctx context.Context, sc Schedule) error {
	var last any
	if sc.LastRun != nil {
		last = strTime(*sc.LastRun)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE schedules SET kind=?, expr=?, message=?, enabled=?, last_run=? WHERE id=?`,
		sc.Kind, sc.Expr, sc.Message, boolInt(sc.Enabled), last, sc.ID)
	return err
}

// MarkScheduleRun records the last execution time.
func (s *Store) MarkScheduleRun(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedules SET last_run=? WHERE id=?`, strTime(at), id)
	return err
}

// DeleteSchedule removes a schedule.
func (s *Store) DeleteSchedule(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id=?`, id)
	return err
}

// --- Audit ---

// Audit writes an audit log entry.
func (s *Store) Audit(ctx context.Context, username, action, detail string) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit (at, username, action, detail) VALUES (?,?,?,?)`,
		strTime(time.Now()), username, action, detail)
}

// ListAudit returns the most recent audit entries.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, username, action, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var at, user, action, detail string
		if err := rows.Scan(&at, &user, &action, &detail); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s  %s  %s  %s", at, user, action, detail))
	}
	return out, rows.Err()
}

// ErrNotFound is returned by helpers when a row is missing.
var ErrNotFound = errors.New("not found")
