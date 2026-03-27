# run

Browser-based terminal at [run.pmh.codes](https://run.pmh.codes).

Authenticates users via OIDC, spins up a Kubernetes pod per user, and streams an interactive shell to the browser using xterm.js over WebSocket.

## How it works

1. User visits the site and is redirected to the OIDC provider to log in
2. After login, a Kubernetes pod is created (or reused) for that user
3. A WebSocket connection bridges the browser terminal to `kubectl exec` inside the pod

Pods are named `run-<sub>-<hash>` using the OIDC `sub` claim as a stable user identifier. A pod in a terminal phase (succeeded/failed) is automatically recreated on next connect.

## Configuration

All configuration is via environment variables.

| Variable | Default | Required | Description |
|---|---|---|---|
| `PORT` | `8080` | | HTTP listen port |
| `BASE_URL` | `https://run.pmh.codes` | | Public base URL, used for the OIDC redirect URI |
| `OIDC_ISSUER_URL` | | yes | OIDC provider issuer URL (must serve `/.well-known/openid-configuration`) |
| `OIDC_CLIENT_ID` | | yes | OAuth2 client ID |
| `OIDC_CLIENT_SECRET` | | yes | OAuth2 client secret |
| `SESSION_SECRET` | | yes | Cookie signing/encryption key, minimum 32 characters |
| `POD_NAMESPACE` | `run` | | Kubernetes namespace where user pods are created |
| `POD_IMAGE` | | yes | Container image for user pods |
| `POD_SHELL` | `/bin/bash` | | Shell binary to exec inside the pod |
| `POD_CPU_LIMIT` | `500m` | | CPU limit per user pod |
| `POD_MEMORY_LIMIT` | `256Mi` | | Memory limit per user pod |
| `KUBECONFIG` | | | Path to kubeconfig file; if unset, uses in-cluster config |

## Deployment

### Prerequisites

- Kubernetes cluster with the app deployed inside it (in-cluster config)
- OIDC provider (e.g. Authentik) with a client configured, redirect URI set to `https://run.pmh.codes/auth/callback`
- Gateway/ingress routing `run.pmh.codes` to the service on port 8080

### Apply manifests

```sh
# Fill in real values first
vim deploy/secret.yaml

kubectl apply -k deploy/
```

The app's `ServiceAccount` is granted a namespaced `Role` with permissions to `get`, `list`, `watch`, `create`, and `delete` pods, and `create` pod exec sessions — scoped to the `run` namespace only.

### Build and push image

```sh
docker build -t ghcr.io/pmh-only/run:latest .
docker push ghcr.io/pmh-only/run:latest
```

## Development

```sh
# Run locally against a kubeconfig
export OIDC_ISSUER_URL=https://auth.example.com/application/o/run/
export OIDC_CLIENT_ID=...
export OIDC_CLIENT_SECRET=...
export SESSION_SECRET=$(openssl rand -hex 32)
export POD_IMAGE=alpine:3.21
export POD_SHELL=/bin/sh
export BASE_URL=http://localhost:8080
export KUBECONFIG=~/.kube/config

go run .
```

## WebSocket protocol

The `/terminal` WebSocket endpoint uses a simple binary framing:

| First byte | Payload |
|---|---|
| `0x00` | Terminal data (stdin from browser, stdout/stderr to browser) |
| `0x01` | Resize event — JSON `{"cols": N, "rows": N}` |
