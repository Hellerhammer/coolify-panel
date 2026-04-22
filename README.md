# coolify-panel

A minimal self-hosted UI that lets selected users start / stop / restart specific
Coolify resources — without giving them access to Coolify itself.

## How it works

```
User ──▶ Traefik ──▶ Authelia/Authentik (forward auth) ──▶ panel ──▶ Coolify API
                      sets Remote-User /                              (only reachable
                      Remote-Groups headers                            internally)
```

- Authentication is fully delegated to Authelia or Authentik (no built-in login).
- The panel container reads `Remote-User` and `Remote-Groups` from the request
  headers and only shows the services allowed for that user/group in `config.yaml`.
- Coolify itself does **not** need to be exposed — the panel reaches the API
  internally over the Coolify Docker network.
- Every action is logged to stdout as an audit trail (user, action, UUID).
- Per-resource **Details** page with:
  - **Environment variables:** optional env-var editing (application-only).
  - **Logs:** for applications, a live log tail (auto-refreshing every 5s) plus a picker for past deployment logs.
  - **Stats:** live container metrics (CPU, Memory, Network) when `docker_host` is configured.
  - **Configuration Files:** editing of specific config files directly within the container when `docker_host` is configured.
- Services and databases do not get log views or env editing through the Coolify API, but can get logs and stats if a `docker_host` is provided.

## Setup

### 1. Create a Coolify API token

In Coolify: **Keys & Tokens → API tokens → New token**.
Scope it to the team that owns the resources you want to control. Needs **write**
access. If you want to use the env-editing feature, the token additionally needs
**`read:sensitive`** — without it, Coolify returns env variables without their
values and the panel will show empty input fields.

### 2. Find the resource UUIDs

Open each resource in Coolify — the UUID is in the URL:
`https://coolify.xxx/project/.../environment/.../application/<UUID>`

Also note whether it's an Application, Service, or Database — that goes into `kind`.

### 3. Create `config.yaml`

See `config.example.yaml`. Per resource:

```yaml
- name: "Factorio Server"         # display name in the panel
  uuid: "abc-123-…"               # Coolify UUID
  kind: applications              # applications | services | databases
  allowed_users: ["alice"]        # matched against Remote-User
  allowed_groups: ["gamers"]      # matched against Remote-Groups (comma-separated)
  actions: [restart, start, stop] # subset
  expose_all_envs: true           # allows editing all environment variables
  # editable_envs: ["KEY1", "KEY2"] # OR: only allow specific variables
  # config_files: ["/path/in/container/config.json"] # requires docker_host
  # docker_host: "tcp://docker-proxy:2375" # enables logs for svc/db, stats, and file editing
```

### 4. Traefik + Authelia

The bundled `docker-compose.yml` assumes you already have Traefik + Authelia running
(exposed as the `authelia@docker` middleware). In Authelia, protect the domain with
at least `one_factor` + 2FA:

```yaml
access_control:
  rules:
    - domain: panel.your-domain.com
      policy: two_factor
      subject: ["group:gamers", "group:admins"]
```

**Authentik** works the same way via a ProxyProvider in forward auth mode — the
header names (`Remote-User`, `Remote-Groups`) are compatible.

### 5. Deploy via Coolify

Easiest path: add the repo (or a zip) to Coolify as a Docker Compose application.
Set `COOLIFY_TOKEN` as a secret env var in Coolify's UI. `config.yaml` is mounted
into the container at `/config/config.yaml` — add a Persistent Storage entry in
Coolify mapping a host path (or volume) to `/config`, and put `config.yaml`
there. Never commit the config with the token embedded.

### 5a. Optional: direct Docker integration (logs, stats, config files)

The bundled `docker-compose.yml` also starts a read-only Docker socket proxy
([Tecnativa's](https://github.com/Tecnativa/docker-socket-proxy)). Setting
`docker_host: "tcp://docker-proxy:2375"` on a resource in `config.yaml` has several benefits:

1.  **Logs for all kinds:** The panel pulls logs straight from the Docker Engine API.
    This is the only way to see logs for **services** and **databases**, as Coolify's
    public API has no log endpoint for them.
2.  **Live Stats:** Enables real-time CPU, Memory, and Network usage metrics in the
    Details page.
3.  **Config File Editing:** Allows users to edit specific files inside the container
    (defined in `config_files`). The panel uses the Docker archive API to read/write
    these files without needing SSH or volume mounts on the panel itself.

Remote hosts work the same way: run a socket proxy on that host and use
`docker_host: "tcp://<host-ip>:2375"`. Never expose the raw Docker socket
unauthenticated — the proxy is not optional.

### 6. Logout with Authentik (forward-auth)

The panel itself can't terminate an Authentik session — the session
cookies are set on the Authentik domain (`auth.domain.tld`), not on the
panel's domain, and browsers won't let one site clear another site's
cookies. So the Logout button only redirects the user to Authentik's
sign-out flow; Authentik does the actual invalidation.

**1. Set `logout_url`** to Authentik's Invalidation Flow:

```yaml
logout_url: "https://auth.your-domain.com/flows/-/default/invalidation/"
```

Do *not* use `/application/o/<slug>/end-session/` — that's the OIDC
end-session endpoint and doesn't exist for Proxy Providers.

**2. Verify the Invalidation Flow.** In Authentik: **Flows & Stages →
Flows → `default-invalidation-flow`**. It needs a `user_logout` stage
(the default). Optionally create a custom flow that, after
`user_logout`, has a **redirect** stage back to `https://panel.domain.tld`
so the user lands somewhere useful instead of Authentik's generic
"signed out" page.

**3. Keep the Outpost's token validity short.** In Authentik: **Providers
→ your Proxy Provider → Token validity**. If this is set to hours or
days, the Outpost caches the auth decision that long, so a logged-out
user would still be let through until the token expires. `minutes=5` or
`minutes=15` is reasonable for a homelab.

**4. Test.**

- `curl -i https://panel.your-domain.com/logout` → expect `302` to
  `logout_url`.
- In a browser: log in, click **Logout** → you should land on Authentik's
  sign-out page.
- Reload `https://panel.your-domain.com` → you must see Authentik's
  login prompt, not the panel.
- Authentik Admin → **Events** → filter for `logout` — your event
  should be there.

If you get redirected straight back into the panel without having to log
in again: the central Authentik session wasn't invalidated (wrong flow
URL), or the Outpost's token cache hasn't expired yet.

### 7. Local testing (no auth)

```bash
COOLIFY_URL=https://your-coolify.com COOLIFY_TOKEN=xxx \
CONFIG_PATH=./config.yaml go run .
# then http://localhost:8080/?user=alice&groups=admins
# (requires dev_mode: true in config)
```

## What it deliberately doesn't do

- No built-in login — an auth proxy is required in production
- No resource creation (limited editing of environment variables and specific config files is supported)
- No team/role hierarchy — just a flat user/group → action map per resource
