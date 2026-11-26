# ChronoHost

A Dockerized dedicated-server manager for **Vintage Story**. ChronoHost wraps game
processes, exposes a 1:1 clone of the in-game *Customize World* screen, caches
official server binaries, installs mods from ModDB, takes scheduled world
snapshots, and streams a live console to the browser.

## Quick start

```bash
cp .env.example .env      # change CHRONOHOST_ADMIN_PASSWORD
docker compose up -d --build
```

Open <http://localhost:8080> and sign in with the credentials from `.env`.
The fallback credentials are `admin` / `chronohost` — set the password before
exposing the portal.

From source, without Docker:

```bash
go run ./cmd/chronohost -data ./data -addr :8080
```

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `CHRONOHOST_DATA` | `/data` | Persistent root (binaries, instances, backups, db). |
| `CHRONOHOST_ADDR` | `:8080` | HTTP listen address. |
| `CHRONOHOST_ADMIN_USER` | `admin` | Bootstrap administrator username. |
| `CHRONOHOST_ADMIN_PASSWORD` | `chronohost` | Bootstrap administrator password. |
| `TZ` | `UTC` | Timezone used by schedules and timestamps. |

The admin variables are read **only on first boot**, when the database has no
users. To change a password later, use **Settings → Users**; deleting the
database resets everything and is destructive.

Discord webhooks are configured in the portal under **Settings**, not via
environment variables, so they can be changed without restarting the container.

## What is included

| Phase | Feature |
| --- | --- |
| 1 | Multi-stage image with Go control plane plus .NET 8 **and** .NET 10 runtimes; official binary downloader with MD5 verification; supervised start/stop/restart/kill with crash watchdog and exponential backoff. |
| 2 | Four-step "Make a Server" wizard mirroring the game client: identity, world generation/geography, survival/combat/temporal mechanics, and a live `serverconfig.json` preview before first boot. |
| 3 | ModDB search/install with compatibility warnings and dependency extraction from `modinfo.json`; scheduled tar.gz snapshots with GFS retention and stash-then-restore. |
| 4 | WebSocket console with command injection, telemetry (players, game day, season, storms, CPU/RAM), three RBAC roles, audit log, and Discord webhooks. |

## Repository layout

```text
cmd/chronohost/       executable entrypoint and signal handling
internal/api/          HTML portal, REST API, WebSocket console, RBAC
internal/backup/       tar.gz snapshots, retention, restore
internal/binaries/     official feeds, MD5-verified downloads, cache
internal/config/       wizard schema, validation, presets, config generator
internal/moddb/        ModDB search, compatibility, dependencies
internal/notify/       Discord webhooks
internal/process/      supervised dotnet process, logs, telemetry, watchdog
internal/scheduler/    cron/@every jobs and in-game warnings
internal/store/        SQLite schema and CRUD
internal/web/          embedded templates and vendored HTMX/Alpine
docker/entrypoint.sh   container defaults and handoff to the binary
```

## Creating a world

1. Open **Make a Server**. Pick one of the five playstyle presets (Standard,
   Wilderness Survival, Exploration, Homo Sapiens, Creative Building) or tune
   every field yourself.
2. Step 1 sets server identity, port, game mode and max clients.
3. Step 2 maps the world generator: seed, dimensions, landcover, upheaval,
   geology, climate, deposits and growth rates.
4. Step 3 maps survival and temporal rules: stability, storms, rifts, health,
   hunger, hardiness, chiseling, hostility, cave-ins and hazards.
5. Step 4 previews the exact JSON that will be written to
   `/data/instances/<id>/serverconfig.json`. Every `WorldConfiguration`
   numeric value is emitted as a **string**, which is required by Vintage
   Story's parser for freshly generated worlds.

Creating an instance writes the config, creates `Saves/` and `Mods/`, installs
any selected mods, adds a default nightly backup schedule, and optionally boots
the server so the world is generated with these settings.

## Ports and persistence

`docker-compose.yml` publishes TCP **and** UDP on `42420-42430`, matching the
default portal port range. Vintage Story 1.20+ uses both protocols. Change the
range in **Settings** and update the Compose mapping together.

All state lives under `/data`:

| Path | Contents |
| --- | --- |
| `/data/binaries` | Cached official server versions (one directory per version). |
| `/data/instances` | One `dataPath` per world: saves, `serverconfig.json`, mods. |
| `/data/backups` | Compressed snapshots and restore metadata. |
| `/data/db` | SQLite database (users, sessions, instances, mods, schedules, audit). |

## Operations

- **Versions** fetches `stable.json`, `unstable.json` and `lateststable.txt`
  from `api.vintagestory.at`; downloads are verified against the published MD5.
  The image carries .NET 8 for 1.21.x servers and .NET 10 for 1.22.x servers.
- **Mods** searches the official ModDB API, prefers releases tagged compatible
  with the instance version, and warns about mismatches or unmet dependencies.
  Removing a mod moves its archive to `ModsDisabled/`.
- **Rollbacks** creates tar.gz snapshots of `.vcdbs` saves, configs and mods.
  Retention (hourly/daily/weekly/monthly counts) is configurable in Settings;
  restore stops the server, stashes the live world, unpacks the snapshot and
  restarts. A `.vcdbs` save must exist before a snapshot can be created.
- **Schedules** accept five-field cron expressions or `@every 6h`, `@daily`,
  etc. Restart jobs broadcast a 60-second in-game warning first.
- **Console** is a WebSocket (`/ws/console?instance=<id>`) with ANSI-stripped
  lines, historical replay, and command injection for operators/admins.
- **Discord** alerts are configured in Settings (webhook URL + enable toggle)
  and cover start/stop/crash, joins/leaves, mod installs and backups.

### Web API

All `/api/*` routes use JSON and the same session cookie as the portal. The
WebSocket console uses `/ws/console?instance=<id>`.

| Area | Representative routes |
| --- | --- |
| Instances | `GET/POST /api/instances`, `POST /api/instances/{id}/start`, `/stop`, `/restart`, `/kill`, `GET /api/instances/{id}/logs`, `GET/PUT /api/instances/{id}/config` |
| Wizard | `POST /api/instances/preview`, `POST /api/instances/preset` |
| Versions | `GET /api/versions`, `POST /api/versions/download`, `DELETE /api/versions/{version}` |
| Mods | `GET /api/moddb/search`, `GET /api/moddb/mod/{id}`, `POST /api/instances/{id}/mods/install` |
| Backups | `GET/POST /api/instances/{id}/backups`, `POST /api/instances/{id}/backups/{bid}/restore`, `DELETE /api/instances/{id}/backups/{bid}` |
| Schedules | `GET/POST /api/instances/{id}/schedules`, `DELETE /api/schedules/{sid}` |
| Admin | `GET/POST /api/users`, `GET/PUT /api/settings`, `GET /api/audit` |

### Roles

| Role | Access |
| --- | --- |
| Viewer | Dashboard, logs, telemetry, config preview, ModDB search. |
| Operator | Viewer + lifecycle, commands, mods, backups, schedules. |
| Admin | Operator + users, settings, config edits, deletion, binary cache. |

At least one admin is always retained. Sessions expire after 24 hours.

## Development

```bash
gofmt -w ./cmd ./internal
go build ./...
go test ./...
go vet ./...
go run ./cmd/chronohost -data ./data -addr :8080
```

Go 1.26+ is required. HTMX and Alpine are vendored under
`internal/web/static/`, so the portal works without CDN access.

## Troubleshooting

- **Server will not start:** open **Versions** and confirm the selected game
  version has a cached binary. 1.21.x needs .NET 8, 1.22.x needs .NET 10; the
  image ships both and the build verifies them. Check the instance log for the
  exact .NET error.
- **World settings look wrong:** world generation settings are only applied
  before the world first boots. Use the Step 4 preview, then start the server
  once to generate the world. Editing `WorldConfiguration` afterwards does not
  retroactively change an existing world.
- **Port conflict:** change the instance port in the wizard or Settings and
  update the `42420-42430` mapping in `docker-compose.yml` to match.
- **Backup fails:** snapshots require at least one `.vcdbs` file in `Saves/`.
  Start the server once so the world is created.
- **Forgot the admin password:** use another admin account under
  **Settings → Users**, or restore access by removing `/data/db/chronohost.db`
  on a stopped container (this resets all users and schedules).

## Health check

`GET /healthz` returns `{"status":"ok"}` and is used by the Docker healthcheck.
