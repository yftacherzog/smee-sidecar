# smee-sidecar

A sidecar container for implementing health checks and monitoring for
[Smee](https://smee.io/) deployments. This sidecar provides active health checking
to verify end to end functionally between the Smee server and a client serving a
specific channel.

## Overview

The smee-sidecar acts as an instrumentation and proxy layer between Smee clients and
downstream services, providing:

- **End-to-end health checking** via round-trip webhook tests
- **Process liveness monitoring** for Kubernetes probes
- **Prometheus metrics** for observability
- **Event relay** with monitoring capabilities

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    Kubernetes Pod                       │
├────────────────────┬────────────────────────────────────┤
│   smee-client      │           smee-sidecar             │
│                    │                                    │
│                    │                                    │
│ ┌─────────────────┐│ ┌─────────────────┐                │
│ │                 ││ │ Relay Server    │                │
│ │   Webhook       ││ │ :8080           │                │
│ │   Events        ││ │                 │                │
│ │                 ││ └─────────────────┘                │
│ └─────────────────┘│ ┌─────────────────┐                │
│                    │ │ Management      │                │
│ Liveness: /healthz │ │ Server :9100    │                │
│                    │ │                 │                │
│                    │ │ /healthz        │ ← Readiness    │
│                    │ │ /livez          │ ← Liveness     │
│                    │ │ /metrics        │ ← Prometheus   │
│                    │ └─────────────────┘                │
└────────────────────┴────────────────────────────────────┘
                             │
                             ▼
                    ┌─────────────────┐
                    │  Downstream     │
                    │  Service        │
                    └─────────────────┘
```

Note that the same approach can also be used for actively verifying end to end
functionality for Smee server deployments.

For that, a server pod will contain a dedicated Smee client container for health checks
and a the smee-sidecar container alongside it, as above.

### How It Works

1. The Smee client is configured to forward events to the sidecar instead of the
   downstream service.
2. The sidecar inspects the events and forwards them to the downstream service, unless
   those are health check events.
3. The sidecar exposes a /healthz endpoint that triggers an end to end test that:
    1. Sends an event to the same server and channel the client subscribes to.
    2. Waits for the event to be forwarded by the client.
    3. If the event arrives, returns success status on the call.
    4. If timed out while waiting, return failure status.
4. The client is configured with the sidecar /healthz endpoint as its liveness probe, so
   the client restarts if the check fails continuously.
5. The sidecar is configured with /livez endpoint as its liveness probe that verifies
   that the server the sidecar is running is functional
   ([more details](#failure-isolation)).

### Metrics

The sidecar exposes Prometheus metrics on `:9100/metrics`:

- `smee_events_relayed_total`: Counter of webhook events successfully relayed
- `health_check`: Gauge indicating the result of the last health check (1=healthy,
   0=unhealthy)

## Configuration

### Environment Variables

|Variable                 |Required|Default|Description                              |
|----------               |--------|-------|-----------                              |
|`DOWNSTREAM_SERVICE_URL` |✅      | -     | Service to relay webhook events to      |
|`SMEE_CHANNEL_URL`       |✅      | -     | Smee channel used by the client         |
|`HEALTHZ_TIMEOUT_SECONDS`|❌      |`20`   | Timeout for end-to-end health checks    |
|`INSECURE_SKIP_VERIFY`   |❌      |`false`| Skip TLS verification for health checks |

### Example Configuration

```yaml
env:
  - name: DOWNSTREAM_SERVICE_URL
    value: "http://my-service:8080"
  - name: SMEE_CHANNEL_URL
    value: "http://smee-server/myawesomechannel"
  - name: HEALTHZ_TIMEOUT_SECONDS
    value: "30"
```

## Kubernetes Deployment

### Complete Example

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app-with-smee
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
        # Main smee client container
        - name: smee-client
          image: ghcr.io/chmouel/gosmee:latest
          args:
            - client
            - "https://smee.io/myawesomechannel"
            - "http://localhost:8080"
          livenessProbe:
            httpGet:
              path: /healthz
              port: 9100
            initialDelaySeconds: 10
            periodSeconds: 10

        # Sidecar container
        - name: sidecar
          image: quay.io/konflux-ci/smee-sidecar:latest
          ports:
            - containerPort: 8080
              name: relay
            - containerPort: 9100
              name: management
          env:
            - name: DOWNSTREAM_SERVICE_URL
              value: "http://localhost:3000"  # Your app
            - name: SMEE_CHANNEL_URL
              value: "https://smee.io/myawesomechannel"
          livenessProbe:
            httpGet:
              path: /livez      # Simple process check
              port: 9100
            initialDelaySeconds: 5
            periodSeconds: 10

        # Your application container
        - name: my-app
          image: my-app:latest
          ports:
            - containerPort: 3000
```

### Failure Isolation

Ideally, we could have had the same endpoint on the sidecar used for liveness probe in
all containers (server, client, sidecar), but in cases the health check fails for
reasons unrelated to the pod itself (e.g. the upstream Smee server is down), all
containers on the pod can experience cascading restart loops, that will not allow them
to be up at the same time to allow the health check to recover.

There are 2 ways to address that:
1. Client deployments: use the **sidecar's** /healthz endpoint as liveness probe for the
   client and the **sidecar's** /livez endpoint as liveness probe for the sidecar.
2. Server deployments: Same as client, but we still need the sidecar's /healthz endpoint
   configured as liveness probe for both the server and the client.

   In this case, we should configure the probe on the server and the client so that
   `failureThreshold` x `initialDelaySeconds` is longer than the max
   backoff penalty on the cluster.

   This will allow the client and the server to be up at the same time and will allow
   the health check to eventually pass when the upstream issue is resolved.

## Image Registry

https://quay.io/repository/konflux-ci/smee-sidecar

## Development

### Building

```bash
# Build the container
docker build -t smee-sidecar:latest .
```

### Testing

```bash
# Run unit tests
go test ./...
```

## Known Limitations

* Only tested with [gosmee](https://github.com/chmouel/gosmee).
