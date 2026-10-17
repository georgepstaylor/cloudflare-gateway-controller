# Cloudflare Gateway Controller

A Kubernetes Gateway API implementation using Cloudflare Tunnels (cloudflared).

## Overview

This controller implements the Kubernetes Gateway API to expose services through Cloudflare Tunnels without requiring public IPs or open inbound firewall ports. It automatically provisions Cloudflare Tunnels, manages DNS records, and configures cloudflared to route traffic to your Kubernetes services based on HTTPRoute definitions.

## Features

-  Native Kubernetes Gateway API support (v1.0.0)
-  GatewayClass, Gateway, and HTTPRoute resources
-  Automatic Cloudflare Tunnel provisioning via API
-  Automatic DNS record management in Cloudflare
-  Dynamic cloudflared configuration updates

## Architecture

```
HTTPRoute → Gateway → cloudflared Deployment → Cloudflare Tunnel → Cloudflare Edge → Internet
```

The controller:
1. Watches Gateway API resources (GatewayClass, Gateway, HTTPRoute)
2. Provisions cloudflared deployments with Cloudflare Tunnel configuration
3. Creates/manages Cloudflare Tunnels via API
4. Automatically creates/updates DNS records for HTTPRoute hostnames
5. Generates cloudflared ingress rules from HTTPRoute path-based routing

## Prerequisites

- Kubernetes cluster (1.27+)
- kubectl configured
- Cloudflare account with:
  - API token with permissions for Tunnels and DNS
  - Account ID
  - At least one domain/zone in Cloudflare

## Installation

The controller is deployed with [Kustomize](https://kustomize.io/); the manifests live in
[`config/`](config/) in this repository. Everything below assumes the default namespace
`cloudflare-gateway-system`.

### 1. Install the Gateway API CRDs

Skip this if they are already installed in the cluster.

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
```

### 2. Install the controller

Apply a tagged release straight from GitHub:

```bash
kubectl apply -k 'github.com/georgepstaylor/cloudflare-gateway-controller/config/default?ref=v0.1.7' # x-release-please-version
```

This creates the namespace, ServiceAccount, ClusterRole, ClusterRoleBinding and the
controller Deployment.

### 3. Create the Cloudflare credentials Secret

The controller reads credentials from a Secret referenced by the GatewayClass's
`parametersRef`, so create it yourself rather than committing it:

```bash
kubectl create secret generic cloudflare-credentials \
  --namespace cloudflare-gateway-system \
  --from-literal=api-token=YOUR_API_TOKEN \
  --from-literal=account-id=YOUR_ACCOUNT_ID
```

### 4. Create a GatewayClass

```bash
kubectl apply -f https://raw.githubusercontent.com/georgepstaylor/cloudflare-gateway-controller/v0.1.7/config/samples/gatewayclass.yaml # x-release-please-version
```

Sample Gateway and HTTPRoute manifests are in [`config/samples/`](config/samples/).

### Migrating from the Helm chart

Earlier versions were installed from a Helm chart, which has been replaced by the
manifests in [`config/`](config/). To move across:

```bash
helm uninstall cloudflare-gateway-controller -n cloudflare-gateway-system
```

Then follow the steps above. Uninstalling the release deletes the Secret it generated, so
recreate it with step 3; your Gateways and HTTPRoutes are untouched, and the controller
picks them back up on startup.

Note that the chart always passed a `--cluster-domain` flag, which no longer exists - the
controller now relies on cluster DNS search paths instead. Replace the Deployment with
these manifests rather than only bumping the image tag, or the manager will exit with
`flag provided but not defined: -cluster-domain`.

### Customising the install

Reference this repository as a base from your own kustomization and patch what you need.
Every knob the Helm chart exposed is either a direct kustomize equivalent or no longer
needed:

| Chart value | Kustomize equivalent |
|-------------|----------------------|
| `image.repository` / `image.tag` | `images:` transformer |
| `imagePullPolicy` | strategic merge patch on the container |
| `replicaCount` | built-in `replicas:` field |
| `resources` | strategic merge patch on the container |
| `podAnnotations` | strategic merge patch on the pod template |
| `imagePullSecrets` | strategic merge patch on the pod spec |
| `nodeSelector` / `tolerations` / `affinity` | strategic merge patch on the pod spec |
| `podSecurityContext` / `securityContext` | strategic merge patch |
| `nameOverride` / `fullnameOverride` | `namePrefix:` / `nameSuffix:` |
| `serviceAccount.name` / `.annotations` | patch the ServiceAccount (and its ClusterRoleBinding subject) |
| `cloudflare.apiToken` / `.accountId` | the Secret you create in step 3 |

`cloudflare.existingSecret`, `gatewayAPI.installCRDs` and `gatewayAPI.version` were never
wired into the chart's templates and have no equivalent here. The Secret name is whatever
your GatewayClass `parametersRef` points at, and the Gateway API CRDs are installed in
step 1.

`cluster.domain` is gone too, and needs no replacement: backend URLs are emitted as
`service.namespace.svc`, which cloudflared's resolv.conf search path completes for
whatever DNS domain your cluster uses. Clusters on a custom domain work with no
configuration at all.

A kustomization exercising most of the above:

```yaml
# kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: cloudflare-gateway-system

resources:
  - github.com/georgepstaylor/cloudflare-gateway-controller/config/default?ref=v0.1.7 # x-release-please-version

images:
  - name: ghcr.io/georgepstaylor/cloudflare-gateway-controller
    newTag: 0.1.7 # x-release-please-version

replicas:
  - name: cloudflare-gateway-controller
    count: 2

patches:
  # Scheduling, annotations, pull secrets and resources in one merge patch.
  - patch: |
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: cloudflare-gateway-controller
      spec:
        template:
          metadata:
            annotations:
              prometheus.io/scrape: "true"
          spec:
            imagePullSecrets:
              - name: ghcr-credentials
            nodeSelector:
              kubernetes.io/arch: arm64
            tolerations:
              - key: node-role.kubernetes.io/control-plane
                operator: Exists
                effect: NoSchedule
            containers:
              - name: manager
                imagePullPolicy: Always
                resources:
                  limits:
                    cpu: 1
                    memory: 256Mi
```

To deploy from a checkout instead:

```bash
make deploy                 # kubectl apply -k config/default
make render                 # print the rendered manifests
make undeploy               # remove it again
```

### Uninstallation

```bash
kubectl delete -k 'github.com/georgepstaylor/cloudflare-gateway-controller/config/default?ref=v0.1.7' # x-release-please-version
```

Gateways created by the controller are cleaned up (and their Cloudflare Tunnels deleted)
when you delete the Gateway resources, so delete those before removing the controller.

## How It Works

### GatewayClass
- Validates controller ownership (`controllerName`)
- References a Secret containing Cloudflare API credentials
- Checks the credentials Secret exists and has `api-token` and `account-id` set (it does
  not call the Cloudflare API, so an invalid token is only discovered when a Gateway is
  reconciled)
- Sets `Accepted` status condition

### Gateway (Represents a Cloudflare Tunnel)
- Creates a new Cloudflare Tunnel via API
- Stores tunnel metadata (ID, secret, name) in Gateway annotations
- Creates a Secret with tunnel credentials JSON
- Creates a ConfigMap with cloudflared configuration YAML
- Deploys cloudflared as a Kubernetes Deployment (2 replicas by default)
- Sets Gateway status with tunnel hostname (`{tunnel-id}.cfargotunnel.com`)
- Handles cleanup: deletes tunnel from Cloudflare when Gateway is deleted

### HTTPRoute (Routes traffic through the Tunnel)
- Watches parent Gateway references
- Extracts hostnames and path-based routing rules
- Builds cloudflared ingress rules mapping hostnames/paths to backend Services
- Updates Gateway's ConfigMap with new ingress configuration
- Creates/updates DNS CNAME records in Cloudflare for each hostname
- Sets HTTPRoute status with parent Gateway conditions

## Development

### Build

```bash
# Build binary
make build

# Run locally (requires kubeconfig)
make run

# Run tests
make test

# Format and vet code
make fmt vet
```

### Docker

```bash
# Build image
make docker-build IMG=your-registry/cloudflare-gateway-controller:tag

# Push image
make docker-push IMG=your-registry/cloudflare-gateway-controller:tag
```

## Troubleshooting

### Controller Logs

```bash
kubectl logs -n cloudflare-gateway-system deployment/cloudflare-gateway-controller -f
```

### Cloudflared Logs

```bash
kubectl logs -l app=cloudflared -f
```

### Common Issues

**Gateway not becoming Ready:**
- Check GatewayClass is `Accepted`
- Verify Cloudflare credentials are valid
- Check controller logs for API errors

**HTTPRoute not working:**
- Verify backend Service exists and has endpoints
- Check Gateway is `Programmed`
- Verify DNS records were created in Cloudflare dashboard
- Check cloudflared logs for routing errors

**DNS records not created:**
- Ensure API token has DNS edit permissions
- Verify zone exists in Cloudflare account
- Check HTTPRoute status conditions

## Limitations

- Only supports HTTPRoute (no TCPRoute, UDPRoute, TLSRoute yet)
- One backend per rule (no weighted load balancing yet)
- PathPrefix and Exact path match types supported
- Cloudflare handles TLS termination at the edge

## Contributing

Contributions welcome! This is a learning project for understanding Kubernetes controllers and the Gateway API.

## License

Apache 2.0

## Acknowledgments

Built with:
- [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)
- [Gateway API](https://gateway-api.sigs.k8s.io/)
- [cloudflare-go](https://github.com/cloudflare/cloudflare-go)
- [cloudflared](https://github.com/cloudflare/cloudflared)
