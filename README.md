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

See the helm chart in [`cloudflare-gateway-controller-helm-charts`](https://github.com/georgepstaylor/cloudflare-gateway-controller-helm-charts) for installation instructions.


## How It Works

### GatewayClass
- Validates controller ownership (`controllerName`)
- References a Secret containing Cloudflare API credentials
- Validates credentials can authenticate with Cloudflare API
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
