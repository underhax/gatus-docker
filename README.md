# Hardened Containerized Gatus

[![CI](https://github.com/underhax/gatus-docker/actions/workflows/ci.yml/badge.svg)](https://github.com/underhax/gatus-docker/actions/workflows/ci.yml)
[![Build](https://github.com/underhax/gatus-docker/actions/workflows/build.yml/badge.svg)](https://github.com/underhax/gatus-docker/actions/workflows/build.yml)
[![GitHub last commit](https://img.shields.io/github/last-commit/underhax/gatus-docker)](https://github.com/underhax/gatus-docker/commits/main)
[![GitHub issues](https://img.shields.io/github/issues/underhax/gatus-docker)](https://github.com/underhax/gatus-docker/issues)
[![GitHub repo size](https://img.shields.io/github/repo-size/underhax/gatus-docker)](https://github.com/underhax/gatus-docker)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

Hardened, zero-trust containerized deployment architecture for [Gatus](https://gatus.io/) running on a minimal scratch container with strict host Discretionary Access Control (DAC) boundaries, in-memory secret mediation, and an embedded zero-dependency healthcheck probe.

## Architecture & Security Model

* **Attack Surface Minimization:** The runtime environment contains only the static application binary, CA trust store, and entrypoint tooling. The absence of shells, package managers, and core utilities prevents post-exploitation persistence, script execution, and tool staging in the event of arbitrary code execution.
* **In-Memory Secret Mediation:** Sensitive configuration parameters (such as [Ntfy](https://github.com/underhax/ntfy-docker) tokens, server URLs, and topics) are ingested directly from mounted secret files into process memory at initialization. This prevents credential exposure in container inspection metadata, process environment dumps, and deployment manifests.
* **Host DAC & Boundary Isolation:** The service runs under a dedicated, unprivileged system account with disabled interactive login. Binding container execution to unique host identifiers enforces strict Discretionary Access Control (DAC) boundaries, preventing lateral movement and file system tampering across adjacent services.
* **Kernel & Container Hardening:** The container root filesystem is mounted strictly read-only, all Linux capabilities are dropped, privilege escalation mechanisms are disabled (`no-new-privileges`), and IPC namespace sharing is restricted.
* **Payload Execution Mitigation:** Persistent data directories holding databases and configuration directories are mounted with non-executable flags (`noexec`, `nosuid`, `nodev`).
* **Perimeter Gateway Isolation:** The service exposes no host network ports. All ingress traffic is mediated and authenticated through an upstream Edge reverse proxy over an isolated container network.
* **Self-Contained Health Telemetry:** Liveness checks are performed by an internal, statically compiled probe binary querying the HTTP health endpoint (`/health`). This eliminates reliance on external network clients (such as `curl` or `wget`) while maintaining continuous container supervision.

---

<details>
<summary><strong>Directory Structure</strong></summary>

Production directory layout on the host (`/home/gatus`):

```text
├── docker-compose.yaml                # Production deployment manifest
├── .env                               # Host infrastructure & server configuration
├── config/                            # Gatus configuration directory
│   └── config.yaml                    # Active configuration file
├── secrets/                           # Docker secrets directory (optional)
│   ├── gatus_ntfy_url                 # Primary Ntfy server URL
│   ├── gatus_ntfy_token               # Primary Ntfy auth token
│   └── gatus_ntfy_topic               # Primary Ntfy topic
└── data/                              # Persistent storage (SQLite database)
    └── data.db                        # SQLite database file
```

</details>

---

<details>
<summary><strong>Deployment &amp; Setup</strong></summary>

### Prerequisites

* **Docker Engine & Compose:** Ensure Docker Engine and the Docker Compose plugin are installed on the host. Follow the official installation guide for [Ubuntu](https://docs.docker.com/engine/install/ubuntu/#install-using-the-repository).
* **External Gateway Network (Optional):** Required only when publishing Gatus externally under a domain name via an [Angie Reverse Proxy](https://github.com/webstudiobond/angie-docker-compose), which automatically provisions the shared `frontend_gateway` network. If deploying standalone without the Angie stack, create it manually:

  ```bash
  docker network create --subnet=172.20.0.0/16 frontend_gateway
  ```

### 1. Create a Dedicated System User

Choose a dedicated, unprivileged numeric ID unique to this service on your host and verify that it is not already in use by another user or group:

```bash
APP_UID=<CHOSEN_UID>

getent passwd ${APP_UID}
getent group ${APP_UID}
```

> **Important:** Both commands must return empty output. Each containerized service must have its own dedicated unprivileged user and group to enforce strict host DAC privilege separation between different stacks.

Create an unprivileged system account with a matching user-group (`-U`) and disabled login shell:

```bash
sudo useradd -m -d /home/gatus -s /usr/sbin/nologin -u ${APP_UID} -U gatus
```

### 2. Create Directory Layout & Assign Ownership

Create the base directory and assign ownership:

```bash
sudo mkdir -p /home/gatus
sudo chown root:gatus /home/gatus
sudo chmod 0750 /home/gatus
```

Create runtime subdirectories from `root` and assign strict DAC permissions:

```bash
sudo mkdir -p /home/gatus/{config,data,secrets}

sudo chown root:gatus /home/gatus/{config,secrets}
sudo chmod 0750 /home/gatus/{config,secrets}

sudo chown gatus:gatus /home/gatus/data
sudo chmod 0700 /home/gatus/data
```

### 3. Deploy Project Files

Download the production manifests directly into `/home/gatus`:

```bash
REPO="https://raw.githubusercontent.com/underhax/gatus-docker/main"

sudo curl -fsSL ${REPO}/docker-compose.yaml -o /home/gatus/docker-compose.yaml
sudo curl -fsSL ${REPO}/.env.example -o /home/gatus/.env
sudo curl -fsSL ${REPO}/config/config.yaml.example -o /home/gatus/config/config.yaml

sudo chown root:root /home/gatus/{docker-compose.yaml,.env}
sudo chmod 0600 /home/gatus/{docker-compose.yaml,.env}

sudo chown root:gatus /home/gatus/config/config.yaml
sudo chmod 0640 /home/gatus/config/config.yaml
```

### 4. Configure Host Environment Settings

Edit `/home/gatus/.env` using a text editor:

```bash
sudo nano /home/gatus/.env
```

Set the host parameters:
* `APP_UID` and `APP_GID`: Matching the numeric UID/GID of your dedicated host `gatus` account.
* `HOST_BIND_IP`: Host interface to bind. Defaults to `127.0.0.1` (loopback only) for private access via SSH tunnel. For WireGuard VPN, set this to the host's WireGuard IP (e.g. `10.8.0.1`).
* `HOST_PORT`: Unique unreserved host port (default `54080`, mapped to container internal port `8080`) to eliminate port collisions with other host services.
* `GATUS_LOG_LEVEL`: Log verbosity (`INFO`, `WARN`, `ERROR`, `DEBUG`).

#### Private Access Recipes

* **Access via SSH Tunnel (Default — 127.0.0.1):**
  Forward the unique host port securely over SSH:
  ```bash
  ssh -i ~/.ssh/id_ed25519 -p 22 -N -L 54080:127.0.0.1:54080 user@your-server-ip
  ```
  Then open `http://localhost:54080` in your local browser.

* **Access via WireGuard VPN:**
  In `/home/gatus/.env`, set `HOST_BIND_IP` to your host's WireGuard interface address (e.g. `HOST_BIND_IP='10.8.0.1'`). Authorized VPN clients can then navigate directly to `http://10.8.0.1:54080`.

* **Access via Edge Angie Reverse Proxy:**
  To publish the UI externally under a domain name via an upstream [Angie Reverse Proxy](https://github.com/webstudiobond/angie-docker-compose), comment out the `ports:` block in `docker-compose.yaml` and uncomment `frontend_gateway` in both places (under `services.gatus.networks` and the top-level `networks:` block).

### 5. Configure Gatus Endpoints & Alerts

Edit `/home/gatus/config/config.yaml`:

```bash
sudo nano /home/gatus/config/config.yaml
```

Configure your endpoints, evaluation conditions, and alerting providers following the [official Gatus documentation](https://gatus.io/docs).

To inject sensitive credentials into the configuration, reference the variables generated by our `init` binary using `${GATUS_<SECRET_NAME>}` placeholders (e.g., `${GATUS_NTFY_URL}`, `${GATUS_NTFY_TOKEN}`). At startup, `init` scans for mounted secret files matching `gatus_*` and exports them into process memory in uppercase. A complete, production-ready template is available in [`config/config.yaml.example`](config/config.yaml.example).

### 6. Configure Docker Secrets

The default deployment manifest is pre-configured with [Ntfy](https://github.com/underhax/ntfy-docker) alert secrets. Initialize the secret files and assign DAC permissions:

```bash
sudo touch /home/gatus/secrets/gatus_ntfy_{url,topic,token}
sudo chown root:gatus /home/gatus/secrets/*
sudo chmod 0440 /home/gatus/secrets/*
```

Populate each secret file:

```bash
sudo nano /home/gatus/secrets/gatus_ntfy_url
sudo nano /home/gatus/secrets/gatus_ntfy_token
sudo nano /home/gatus/secrets/gatus_ntfy_topic
```

#### Adding Additional Secrets (e.g. Matrix)

Secrets are not limited to Ntfy. Any additional credential can be ingested by creating the secret file, registering it in Compose, and referencing it in the configuration:

1. **Create the secret files on the host:**
   ```bash
   sudo touch /home/gatus/secrets/gatus_matrix_{server_url,access_token,room}
   sudo chown root:gatus /home/gatus/secrets/*
   sudo chmod 0440 /home/gatus/secrets/*

   sudo nano /home/gatus/secrets/gatus_matrix_server_url
   sudo nano /home/gatus/secrets/gatus_matrix_access_token
   sudo nano /home/gatus/secrets/gatus_matrix_room
   ```

2. **Mount the secrets in `docker-compose.yaml`:**
   ```yaml
   services:
     gatus:
       secrets:
         - gatus_matrix_server_url
         - gatus_matrix_access_token
         - gatus_matrix_room

   secrets:
     gatus_matrix_server_url:
       file: ./secrets/gatus_matrix_server_url
     gatus_matrix_access_token:
       file: ./secrets/gatus_matrix_access_token
     gatus_matrix_room:
       file: ./secrets/gatus_matrix_room
   ```

3. **Reference the variables in `config.yaml`:**
   ```yaml
   alerting:
     matrix:
       server-url: "${GATUS_MATRIX_SERVER_URL}"
       access-token: "${GATUS_MATRIX_ACCESS_TOKEN}"
       internal-room-id: "${GATUS_MATRIX_ROOM}"
   ```

### 7. Host Permissions &amp; Privilege Separation

```bash
sudo chown root:gatus /home/gatus
sudo chmod 0750 /home/gatus

sudo chown root:root /home/gatus/docker-compose.yaml /home/gatus/.env
sudo chmod 0600 /home/gatus/docker-compose.yaml /home/gatus/.env

sudo chown root:gatus /home/gatus/config/config.yaml
sudo chmod 0640 /home/gatus/config/config.yaml

sudo chown -R root:gatus /home/gatus/secrets
sudo find /home/gatus/secrets -type d -exec chmod 0750 {} +
sudo find /home/gatus/secrets -type f -exec chmod 0440 {} +

sudo chown -R gatus:gatus /home/gatus/data
sudo find /home/gatus/data -type d -exec chmod 0700 {} +
sudo find /home/gatus/data -type f -exec chmod 0600 {} +
```

### 8. Start the Stack

Pull the latest release and start the service in detached mode:

```bash
sudo docker compose -f /home/gatus/docker-compose.yaml pull
sudo docker compose -f /home/gatus/docker-compose.yaml up -d
```

</details>

---

<details>
<summary><strong>Operation &amp; Maintenance</strong></summary>

### Verifying Service Health

Check container status and resource consumption:

```bash
sudo docker compose -f /home/gatus/docker-compose.yaml ps
sudo docker stats --no-stream gatus
```

### Inspecting Runtime Logs

Follow service logs in real time:

```bash
sudo docker compose -f /home/gatus/docker-compose.yaml logs -f
```

### Updating the Image

Update to the latest release and recreate the container:

```bash
sudo docker compose -f /home/gatus/docker-compose.yaml pull
sudo docker compose -f /home/gatus/docker-compose.yaml up -d
```

### Tearing Down the Stack

Stop and remove the container:

```bash
sudo docker compose -f /home/gatus/docker-compose.yaml down
```

</details>

---

<details>
<summary><strong>Development</strong></summary>

For local binary compilation, unit testing, static analysis, and CI/CD pipeline details, see **[DEVELOPMENT.md](DEVELOPMENT.md)**.

</details>

---

<details>
<summary><strong>License &amp; Attribution</strong></summary>

## License & Attribution

This repository and deployment architecture are licensed under the [MIT License](LICENSE). This project is an independent containerized deployment architecture and is not affiliated with, endorsed, or sponsored by TwiN.

**Gatus License & Ownership:** Gatus is open-source software licensed under the [Apache License 2.0](https://github.com/TwiN/gatus/blob/master/LICENSE) and developed by [TwiN](https://github.com/TwiN).

</details>
