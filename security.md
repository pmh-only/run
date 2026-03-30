# Security Report — run.pmh.codes

## Overview

`run.pmh.codes` is a browser-based terminal service that authenticates users via OIDC and gives each user an isolated, persistent Linux environment running as a Kubernetes pod. This document describes the security architecture, the controls in place at each layer, and the known residual risks.

---

## Architecture Summary

```
Browser → Envoy Gateway (TLS) → Go app (run) → kubectl exec → User pod (systemd, Ubuntu 24.04)
                                     ↑
                               Authentik OIDC
```

Each user pod runs systemd as PID 1, has a dedicated PVC for persistent storage, and executes the user's shell session via `su - <username>` inside the container.

---

## Layer 1 — Authentication

### What is in place

- **OIDC via Authentik**: All access requires a valid OIDC login. The app uses `coreos/go-oidc` with full ID token verification (signature, issuer, audience, nonce, expiry).
- **OAuth2 state and nonce**: CSRF protection via a 128-bit random state parameter; replay protection via a 128-bit nonce bound to the ID token.
- **Session cookie**: Signed (HMAC-SHA256) and encrypted (AES-256) gorilla/sessions cookie. `HttpOnly`, `Secure`, `SameSite=Lax`. 24-hour TTL.
- **RequireAuth middleware**: Applied to all non-public routes. Checks both `user_sub` and `username`; redirects to `/auth/login` if either is missing.
- **Username sanitization**: `preferred_username` from the OIDC token is lowercased, stripped to `[a-z0-9_]`, and truncated to 32 characters before being used as a Linux username. Numeric-leading names are prefixed with `user_`.

### Residual risks

- **Session secret rotation**: If `SESSION_SECRET` is rotated, all active sessions are immediately invalidated (good), but there is no graceful invalidation mechanism.
- **Token refresh**: The access token is not stored or refreshed. The session is valid for 24 hours regardless of what happens to the OIDC session at Authentik. Revoking a user at the IdP does not immediately terminate their active session.
- **No MFA enforcement at app level**: MFA is controlled entirely by Authentik. The app trusts whatever the IdP asserts.

---

## Layer 2 — Network

### What is in place

- **TLS termination at Envoy Gateway**: All external traffic is HTTPS/WSS. The Go app listens on plain HTTP inside the cluster only.
- **WebSocket origin check**: The `/terminal` WebSocket endpoint validates the `Origin` header against the configured `BASE_URL`. Cross-origin connections are rejected.
- **Kubernetes NetworkPolicy** (`run-pod-egress`): User pods (label `app: run`) are denied egress to RFC 1918 ranges `10.0.0.0/8` and `172.16.0.0/12` via the native K8s NetworkPolicy API.
- **nftables enforcement** (`run-netpol` DaemonSet): A privileged DaemonSet runs on every node and enforces egress at the kernel level using nftables `mangle FORWARD` rules. It polls the Kubernetes API every 5 seconds to track pod IPs and updates the nftables set atomically. This provides enforcement even if CNI-level policy is bypassed or misconfigured.
- **DNS locked to Cloudflare**: User pods use `1.1.1.1` as their only nameserver (`DNSPolicy: None`). Cluster DNS is not accessible.
- **ServiceAccount token not mounted**: `automountServiceAccountToken: false` prevents pods from querying the Kubernetes API.

### Residual risks

- **192.168.0.0/16 not blocked**: The nftables and NetworkPolicy rules block `10.0.0.0/8` and `172.16.0.0/12` but not `192.168.0.0/16`. If any host or service uses that range, a user pod can reach it. This is the most actionable open gap.
- **No IPv6 egress policy**: Neither the NetworkPolicy nor the nftables rules cover IPv6. Pods that obtain an IPv6 address could reach internal services over IPv6 with no restriction.
- **No ingress restriction**: The NetworkPolicy only specifies `Egress`. There is no `Ingress` policy on user pods. Another pod in the cluster could initiate connections to a user pod.
- **nftables polling window**: There is a ~5 second window after a new pod IP is assigned before the nftables set is updated. During this window a newly started pod has unrestricted egress at the kernel level (the K8s NetworkPolicy still applies immediately).
- **No egress restriction on the run app itself**: The server pod has no egress NetworkPolicy. If the app is compromised it can reach any internal address.

---

## Layer 3 — Pod Isolation

### What is in place

- **One pod per user**: Each authenticated user receives a dedicated pod. Pods are named using a SHA-256 hash of the OIDC `sub` claim, making names stable and collision-resistant.
- **Non-root user session**: The terminal is executed as the user's own Linux account (`su - <username>`) at UID 1000. Users do not connect as root.
- **Passwordless sudo**: Users have `NOPASSWD:ALL` sudo access within their own pod. This is intentional — the pod is the user's personal environment, not a shared system.
- **CPU and memory limits**: Hard limits are enforced via Kubernetes resource limits (`500m` CPU, `256Mi` memory by default). No requests are set, so the scheduler can place pods on any node.
- **Persistent storage**: Each user has a dedicated PVC (`local-path` storage class, `ReadWriteOnce`). The PVC is not shared between users.
- **No service account token**: `automountServiceAccountToken: false` prevents the user from calling the Kubernetes API using the pod's identity.
- **Restart policy Never**: Pods do not automatically restart on failure, preventing runaway crash-loop resource consumption.

### Residual risks

- **User is effectively root inside their environment**: Passwordless sudo means a user can do anything inside their pod (install software, run privileged binaries, etc.). This is by design but means pod-level compromise equals full environment compromise.
- **No storage quota enforcement**: There is no LimitRange or quota on PVC size beyond the initial request. A user could fill the PVC and potentially cause storage pressure on the underlying node.
- **Shared node**: All user pods run on the same k3s nodes. A container escape would give access to other users' pod processes and PVC data on that node.
- **Pod name annotation leaks `user_sub`**: The OIDC `sub` claim is stored in the pod annotation `run/user-sub`. Anyone with `kubectl get pod` access in the `run` namespace can read this value.

---

## Layer 4 — Container Security

### What is in place

- **Reduced Linux capabilities** (13 of 40): Only capabilities required for systemd are granted. Removed from the original set: `SYS_PTRACE`, `DAC_READ_SEARCH`, `SETFCAP`, `SETPCAP`, `SYS_CHROOT`, `IPC_LOCK`, `SYS_BOOT`.

  Granted capabilities:
  | Capability | Purpose |
  |---|---|
  | `SYS_ADMIN` | cgroup v2, mount, namespace operations |
  | `NET_ADMIN` | network interface management |
  | `SYS_RESOURCE` | `setrlimit` for service resource limits |
  | `SETUID` / `SETGID` | service user switching |
  | `KILL` | process lifecycle management |
  | `CHOWN` / `DAC_OVERRIDE` / `FOWNER` / `FSETID` | file permission management during boot |
  | `MKNOD` | `/dev` device node creation |
  | `NET_BIND_SERVICE` | binding to ports below 1024 |
  | `AUDIT_WRITE` | PAM authentication logging |

- **AppArmor profile `run-user`**: A custom localhost AppArmor profile is applied to every user container. It:
  - Allows all filesystem access within the container (required for a general-purpose environment)
  - Restricts mount operations to specific filesystems and options required by systemd
  - Denies raw access to host block devices (`/dev/sd*`, `/dev/nvme*`, `/dev/vd*`, `/dev/hd*`)
  - Removes `ptrace` and `dac_read_search` that were previously allowed
  - Is distributed to all 3 nodes via a DaemonSet and reloaded hourly

- **Seccomp: Unconfined**: Seccomp is explicitly set to `Unconfined` at the container level. This was required because systemd makes a large number of syscalls that are blocked by the default container seccomp profile.

- **`/dev/kmsg` bind-mounted from host**: Required for systemd journal. The mount is read-only at the kernel level (kmsg device).

- **Service account token not mounted**: Confirmed also at the container security level via `automountServiceAccountToken: false` on the pod spec.

### Residual risks

- **`SYS_ADMIN` is highly privileged**: Required for cgroup v2 and mount operations by systemd, but also enables many dangerous operations (mounting filesystems, modifying namespaces, interacting with kernel subsystems). This is the most significant capability risk and cannot be eliminated without dropping systemd.
- **Seccomp Unconfined**: No syscall filtering is applied. A user can invoke any syscall that the kernel allows given the other constraints (capabilities + AppArmor). A custom seccomp profile tuned for systemd would reduce this risk but requires ongoing maintenance.
- **AppArmor allows all filesystem read/write inside container**: The `/** rwlkix` rule provides no file-level isolation within the container. Users can read and modify all files in their own environment, including system binaries — which is expected behavior, but means a compromised binary within the container is indistinguishable from legitimate use.
- **`/dev/kmsg` exposes kernel log**: Users can read host kernel messages via `/dev/kmsg`, which may leak information about other workloads or kernel events on the same node.

---

## Layer 5 — Application Security

### What is in place

- **RBAC scoped to `run` namespace**: The `run-app` ServiceAccount has a `Role` (not `ClusterRole`) granting only `get`, `list`, `watch`, `create`, `delete` on pods and `create` on `pods/exec` — strictly within the `run` namespace.
- **No direct shell injection risk in pod naming**: Pod names are derived from a SHA-256 hash of the OIDC `sub` claim and sanitized through a regex. The pod name is never constructed from user-supplied input that goes to a shell.
- **Username injection protection**: The Linux username passed to the `setup-user` init container is formatted with Go's `%q` verb, which escapes shell special characters. The username also passes through `sanitizeUsername` which limits the character set to `[a-z0-9_]`.
- **WebSocket binary framing**: The terminal protocol uses a simple binary framing layer (`0x00` = data, `0x01` = resize). There is no command injection surface at the protocol level.
- **Static files served directly**: xterm.js and CSS are served from the local filesystem, not from a CDN, for the app-owned files. (CDN links are used for xterm.js library itself — see residual risks.)

### Residual risks

- **CDN-hosted xterm.js**: `xterm.min.js` and `xterm-addon-fit.min.js` are loaded from `cdn.jsdelivr.net`. A compromised CDN response could execute arbitrary JavaScript in the user's browser with full terminal access. Subresource Integrity (SRI) hashes are not set on these script tags.
- **No Content Security Policy**: There is no `Content-Security-Policy` header on responses. XSS in the terminal UI (e.g. via a malicious escape sequence interpreted as HTML by the browser) would have no mitigation.
- **No rate limiting on pod creation**: Any authenticated user can trigger pod creation on every request to `/terminal`. There is no throttling, so a user could repeatedly close and reconnect to generate pod churn.
- **RBAC allows delete on all pods in namespace**: The `run-app` role can delete any pod in the `run` namespace, not just pods it created. This means a bug in the app could delete infrastructure pods (`run-apparmor`, `run-netpol`) if they ran in the same namespace.

---

## Summary Table

| Area | Control | Strength | Gap |
|---|---|---|---|
| Authentication | OIDC + signed/encrypted session | Strong | No token refresh / revocation propagation |
| Network (external) | TLS via Envoy, WSS only | Strong | — |
| Network (egress) | NetworkPolicy + nftables | Strong | Missing 192.168.0.0/16, no IPv6 |
| Network (ingress) | None on user pods | Weak | No ingress policy |
| Pod isolation | Per-user pod + PVC | Strong | Shared node (no node isolation) |
| User identity | UID 1000, named user, sudo | Good | Sudo = effectively root in pod |
| Capabilities | 13 of 40 retained | Moderate | `SYS_ADMIN` unavoidable with systemd |
| Seccomp | Unconfined | Weak | No syscall filtering |
| AppArmor | Custom `run-user` profile | Moderate | Full FS access inside container |
| Application RBAC | Namespace-scoped Role | Strong | Can delete infra pods in same namespace |
| Frontend supply chain | CDN for xterm.js | Weak | No SRI hashes, no CSP |
