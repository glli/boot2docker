# Docker Bridge Proxy (Windows to VM) v1.0

A lightweight, zero-dependency Go utility that bridges the gap between Windows Docker clients and a Docker daemon running inside a Virtual Machine (VMware, VirtualBox, etc.).

## Key Features
* **Path Translation:** Automatically converts Windows-style paths (e.g., `C:\data@) to Linux-style mount points (e.g., `/mnt/hgfs/docker/volumes/C/data@).
* **Auto-Tunneling:** Intercepts container creation calls and dynamically opens TCP tunnels for any published ports (essential for Supabase, Postgres, etc.).
* **Environment Variable Rewriting:** Fixes Windows paths inside `Env` variables sent to containers.
* **No Admin Required:** Uses standard TCP sockets (127.0.0.1), avoiding the permission issues often found with Windows Named Pipes.

---

## Build Instructions

Since this tool uses only the Go standard library, it is easy to compile:

1. **Create a Project Directory:**

   `mkdir docker-proxy`

   `cd docker-proxy`

   `go mod init docker-proxy`
2. **Save the Code:** Save the provided Go source code as `main.go` in this directory.
3. **Compile:**

   `go build -o docker-proxy.exe main.go`

---

## Usage Guide

### 1. Launch the Proxy
Open a dedicated terminal and start the proxy. It must stay running while you are working.

**For VMware (Default):**

`./docker-proxy.exe -ip 192.168.137.25`

**For VirtualBox:**

`./docker-proxy.exe -ip 192.168.56.101 -base /`


### 2. Configure your Terminal
You need to tell your Docker CLI to talk to the local proxy instead of the default Windows pipe.

**Option A: Temporary (Current Session)**

`$env:DOCKER_HOST = "tcp://127.0.0.1:2375"`


**Option B: Permanent (Recommended):*

`docker context create vm-bridge --docker "host=tcp://127.0.0.1:2375"`

`docker context use vm-bridge`


### 3. Run your Tools
Now run your CLI tools as if you were on native Linux.

* **Supabase:** `supabase start` (Ports 54321-54330 will tunnel automatically).
* **Docker Volumes:** `docker run -v C:\my-code:/app alpine ls /app`

---

## Configuration Flags

| Flag | Default | Description |
| :--- | :--- | :--- |
| -ip | 192.168.137.25 | The actual IP address of your Linux Guest VM. |
| -vm-port | 2375 | The Docker port where Docker is listening inside the VM. |
| -local-port | 2375 | The prort the proxy opens on your Windows host. |
| -base | /mnt/hgfs/docker/volumes/ | Guest path where Windows drives are shared/mounted. |

---

## Troubleshooting

### Port Conflicts
If you see `[TUNNEL] Port 54322 is already occupied`, it means a local service (like a local Postgres installation) is already using that port. Stop the local service to allow the proxy to tunnel VM traffic to that port.

### Connection Refused
Ensure your VM's Docker daemon is configured to listen on the network. Inside the VM, your `/lib/systemd/system/docker.service` include:
`ExecStart=/usr/bin/dockerd -H fd:// -H tcp://0.0.0.0:2375`

### Path Mismatches
If your files aren't appearing in the container, check the proxy console logs. You will see lines like:
`[PATH] Rewrote Bind: C:\Data -> /mnt/hgfs/docker/volumes/C/Data@d
Ensure that the path `/mnt/hgfs/docker/volumes/C/` actually exists and is mounted inside your Linux VM.
