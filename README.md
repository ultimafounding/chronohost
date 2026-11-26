# ChronoHost

A calm control room for **Vintage Story** dedicated servers.

ChronoHost runs the game for you, downloads official server binaries, gives you
a world-creation wizard that mirrors the in-game screens, installs mods from
ModDB, takes backups while you sleep, and puts a live console in your browser.
It is one small Go binary in one Docker image, with everything it needs to keep
your worlds running.

## Start a server

You need Docker with the Compose plugin. ChronoHost publishes prebuilt images to
GitHub Container Registry, so no source checkout is required.

```bash
mkdir chronohost && cd chronohost

curl -fsSLO https://raw.githubusercontent.com/ultimafounding/chronohost/main/docker-compose.yml
curl -fsSL -o .env https://raw.githubusercontent.com/ultimafounding/chronohost/main/.env.example

# Edit .env and choose a strong admin password before exposing the portal.
${EDITOR:-nano} .env

docker compose up -d
```

Open <http://localhost:3620>. Sign in with the credentials from `.env`.

If you skip the edit, the fallback login is `admin` / `chronohost`. That is
fine for a quick test on your own machine, not for anything reachable from the
internet.

The first login happens only when the database is empty. Later password changes
belong in **Settings -> Users**. Deleting the database resets everything, so
treat `/data/db/chronohost.db` as precious cargo.

### Build from source instead

```bash
git clone https://github.com/ultimafounding/chronohost.git
cd chronohost
cp .env.example .env
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
```

The build override compiles the local Go source. By default, Compose uses the
published image from GitHub Releases.

## Releases and images

Every `vX.Y.Z` tag triggers a GitHub Actions release. It publishes:

- `ghcr.io/ultimafounding/chronohost:vX.Y.Z`
- `ghcr.io/ultimafounding/chronohost:X.Y.Z`, `X.Y` and `X`
- `ghcr.io/ultimafounding/chronohost:latest`
- A Linux amd64 binary attached to the GitHub release
- Generated release notes and a short run command on the release page

To upgrade an existing install:

```bash
docker compose pull
docker compose up -d
```

Release history lives at
<https://github.com/ultimafounding/chronohost/releases>. The image carries both
.NET 8 and .NET 10, so you can switch between 1.21.x and 1.22.x servers without
rebuilding anything.

## What you get

| Area | What it does |
| --- | --- |
| Dashboard | Start, stop, restart and kill worlds. Watch online players, game day, season, storms, CPU and memory. |
| World wizard | Four steps covering identity, terrain, survival rules and a final JSON preview. Presets get you going quickly; every setting stays editable. |
| Versions | Fetches official stable and unstable feeds, downloads verified binaries and caches them under `/data/binaries`. |
| Mods | Searches ModDB, checks version compatibility, extracts dependencies and handles enable/disable. |
| Backups | Scheduled tar.gz snapshots with grandfathered retention and a restore flow that stashes the live world first. |
| Console | Live WebSocket log with history and command input for operators and admins. |
| Access control | Viewer, operator and admin roles, an audit trail, and Discord alerts for the interesting moments. |

## Make a world

1. Open **Make a Server**. Choose Standard, Wilderness Survival, Exploration,
   Homo Sapiens, Creative Building, or start from a blank slate.
2. **Step 1** sets the name, description, password, port, game mode and player
   limit.
3. **Step 2** covers seed, world size, landcover, climate, geology, deposits and
   growth.
4. **Step 3** covers survival and time: stability, storms, rifts, health, hunger,
   hostility, cave-ins and hazards.
5. **Step 4** shows the exact `serverconfig.json` that will be written. Start the
   server once to generate the save with those settings.

World generation settings are applied before the world first boots. Changing
`WorldConfiguration` later does not rewrite an existing world; that is a game
rule, not a ChronoHost quirk. Use a new world or restore a backup when you want
to reroll the dice.

Every generated config includes Vintage Story's stock player roles. Older
configs that omitted them are repaired automatically at startup, which prevents
the fatal `default group code suplayer but no such group exists` error.

## Ports and storage

Compose publishes the portal on `3620` and both TCP and UDP game traffic on
`42420-42430`. Vintage Story 1.20+ uses both protocols. If you change the range
in **Settings**, update the Compose mapping to match.

| Path | Contents |
| --- | --- |
| `/data/binaries` | Cached server versions, one directory per version. |
| `/data/instances` | One data path per world: saves, mods and `serverconfig.json`. |
| `/data/backups` | Compressed snapshots and restore metadata. |
| `/data/db` | SQLite database with users, sessions, instances, mods, schedules and audit events. |

All of it lives in a single named volume, `chronohost-data`. Back up that volume
if you care about your worlds.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `CHRONOHOST_DATA` | `/data` | Persistent root for all state. |
| `CHRONOHOST_ADDR` | `:3620` | HTTP listen address. |
| `CHRONOHOST_ADMIN_USER` | `admin` | Bootstrap administrator username. |
| `CHRONOHOST_ADMIN_PASSWORD` | `chronohost` | Bootstrap administrator password. |
| `TZ` | `UTC` | Timezone for schedules and timestamps. |

Discord webhooks are configured in **Settings**, not through environment
variables, so you can change them without restarting the container.

## Operations notes

- Schedules accept five-field cron expressions and friendly forms such as
  `@every 6h` or `@daily`. Restart jobs warn players 60 seconds ahead.
- Backups need at least one `.vcdbs` save file. Start the world once before
  taking your first snapshot.
- Disabled mods move to `ModsDisabled/` rather than being deleted.
- The console is available at `/ws/console?instance=<id>`. Viewers can read;
  operators and admins can send commands.
- `GET /healthz` returns `{"status":"ok"}` and powers the container healthcheck.

### Roles

| Role | Can do |
| --- | --- |
| Viewer | Dashboard, logs, telemetry, config preview and ModDB search. |
| Operator | Everything a viewer can, plus lifecycle, commands, mods, backups and schedules. |
| Admin | Everything an operator can, plus users, settings, config edits, deletion and the binary cache. |

Sessions expire after 24 hours. ChronoHost always keeps at least one admin.

### Web API

All `/api/*` routes use JSON and the portal session cookie.

| Area | Example routes |
| --- | --- |
| Instances | `GET/POST /api/instances`, `POST /api/instances/{id}/start`, `/stop`, `/restart`, `GET/PUT /api/instances/{id}/config` |
| Wizard | `POST /api/instances/preview`, `POST /api/instances/preset` |
| Versions | `GET /api/versions`, `POST /api/versions/download` |
| Mods | `GET /api/moddb/search`, `POST /api/instances/{id}/mods/install` |
| Backups | `GET/POST /api/instances/{id}/backups`, `POST /api/instances/{id}/backups/{bid}/restore` |
| Admin | `GET/POST /api/users`, `GET/PUT /api/settings`, `GET /api/audit` |

## Development

Go 1.26+ is required. HTMX and Alpine are vendored in `internal/web/static/`, so
the portal does not call a CDN at runtime.

```bash
gofmt -w ./cmd ./internal
go build ./...
go test ./...
go vet ./...
docker build --check .
docker build -t chronohost:local .
go run ./cmd/chronohost -data ./data -addr :3620
```

The source is split by job: `internal/api` for the portal and API,
`internal/process` for supervision and telemetry, `internal/config` for the
wizard, `internal/binaries` for official downloads, `internal/moddb` for mods,
`internal/backup` for snapshots, `internal/notify` for Discord,
`internal/scheduler` for cron jobs, and `internal/store` for SQLite.

## Troubleshooting

- **Server will not start.** Check **Versions** for the selected binary. The
  image includes .NET 8 for 1.21.x and .NET 10 for 1.22.x; the instance log has
  the exact error.
- **`default group code suplayer but no such group exists`.** Upgrade to a build
  with this fix. ChronoHost adds the missing stock roles on startup.
- **World settings look wrong.** They only apply before the first world boot.
  Use the Step 4 preview and start the world once to generate the save.
- **Port conflict.** Change the instance port in the wizard or Settings and
  update the `42420-42430` mapping in Compose.
- **Backup fails.** Start the server once so a `.vcdbs` save exists.
- **Forgot the admin password.** Sign in with another admin and change it in
  **Settings -> Users**, or stop the container and remove
  `/data/db/chronohost.db`. The second option resets all users and schedules.
