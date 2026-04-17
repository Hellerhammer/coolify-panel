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

## Setup

### 1. Create a Coolify API token

In Coolify: **Keys & Tokens → API tokens → New token**.
Scope it to the team that owns the resources you want to control. Needs write access.

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
Set `COOLIFY_TOKEN` as a secret env var in Coolify's UI. Mount `config.yaml` as a
persistent file, or commit it to the repo (just never with the token in it).

### 6. Local testing (no auth)

```bash
COOLIFY_URL=https://your-coolify.com COOLIFY_TOKEN=xxx \
CONFIG_PATH=./config.yaml go run .
# then http://localhost:8080/?user=alice&groups=admins
# (requires dev_mode: true in config)
```

## What it deliberately doesn't do

- No built-in login — an auth proxy is required in production
- No resource creation or config editing — only start/stop/restart
- No log viewer (possible via API, but expands the attack surface)
- No team/role hierarchy — just a flat user/group → action map per resource
