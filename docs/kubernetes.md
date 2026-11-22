# Deploy Chains to Kubernetes

The authority, broker, and each standalone integration run in separate Pods. The authority owns the SQLite database and may run rules itself. The demo runs one authority, one Mosquitto broker, one simulated fan, and one sensor. It uses an anonymous broker intended for a disposable test cluster.

## Build and configure

Build and publish the [Go and Python images](docker-compose.md#docker-images-and-compose). Set the image names and tags with Kustomize's `images` field in your deployment overlay. The examples use `chains:dev` and `chains-python:dev`. A registry-backed overlay can override the image as follows.

```yaml
images:
  - name: chains
    newName: registry.example.org/chains/engine
    newTag: "0.1.0"
```

For a disposable [kind cluster](https://kind.sigs.k8s.io/docs/user/quick-start/#loading-an-image-into-your-cluster), load the locally built images with `kind load docker-image chains:dev chains-python:dev --name YOUR_CLUSTER`.

The authority claims 1 GiB on the default StorageClass. If your cluster has no default, set `storageClassName` on the PVC to a suitable class. The volume must support SQLite's file-locking semantics and allow the authority to write as UID and GID 10001. Keep one authority replica. Its `Recreate` deployment replaces the old Pod before starting its replacement. The same claim stores published web rules at `/var/lib/chains/state.sqlite.rules`. The ConfigMap remains the source of deployment-managed rules.

## Start the demo

Use the intended cluster context and a dedicated namespace. Set `CHAINS_TOKEN` to an operator token in your terminal.

```sh
export CHAINS_TOKEN="$(openssl rand -hex 32)"
kubectl create namespace chains-demo
printf 'CHAINS_BUS=mqtt://mosquitto:1883\nCHAINS_TOKEN=%s\n' "$CHAINS_TOKEN" |
  kubectl -n chains-demo create secret generic chains-runtime --from-env-file=/dev/stdin
kubectl -n chains-demo apply -k deploy/kubernetes/demo
kubectl -n chains-demo rollout status deploy/chains
kubectl -n chains-demo rollout status deploy/chains-fan
kubectl -n chains-demo rollout status deploy/chains-sensor
kubectl -n chains-demo get pods
kubectl -n chains-demo port-forward --address 127.0.0.1 svc/chains 8080:8080
```

Open `http://127.0.0.1:8080/ui/` and sign in. After the fan reports power off and the sensor appears in State, publish a reading from another terminal.

```sh
kubectl -n chains-demo exec deploy/mosquitto -- \
  mosquitto_pub -q 1 -t sensors/bathroom/humidity -m 72.5
kubectl -n chains-demo logs deploy/chains-fan
```

The fan should report power on. Restarting the authority retains the last humidity reading on the PVC. Deleting the namespace also deletes its claim. Back up SQLite state before decommissioning a deployment whose data you want to retain.

## Use an existing broker

Use `deploy/kubernetes/base` as a Kustomize resource in your own overlay. Set the `chains-runtime` Secret's `CHAINS_BUS` to a reachable MQTT or Redis URL and set `CHAINS_TOKEN` separately. The base creates the authority, its ClusterIP operator Service, the state claim, and its rules and configuration. Edit or replace the generated ConfigMap inputs in your overlay for site-specific rules and HTTP profiles, then deploy with `kubectl -n YOUR_NAMESPACE apply -k YOUR_OVERLAY`.

For remote access, route the operator Service through your HTTPS ingress or reverse proxy. Preserve the public `Host` and disable buffering for SSE on `/operator/v1/activity`. The loopback proxy setting is for locally forwarded demos. See [web console access](operator-web.md).

Run each additional integration in its own Deployment. Give it a stable service ID, the same bus and namespace, and only the secrets it uses. Mount its configuration from a ConfigMap or Secret and a dedicated volume if it keeps local state. For a separate rules worker, use the Go image with `engine`, a unique `-id`, `-authority engine`, and its own rule files with operator HTTP disabled. Install each action-producing rule on one engine. The [concurrency guide](rules-engine-concurrency.md#splitting-work-across-engine-processes) describes how workers synchronize state.

The [Onkyo service](onkyo.md#containers) uses a separate Deployment with LAN access to the receiver.

## Schedule an integration on a hardware node

Label the machine that has the RFLink USB gateway:

```sh
kubectl label node YOUR_NODE chains-automation.io/rflink=true
```

The [RFLink template](../deploy/kubernetes/rflink/rflink.yaml) uses that label to place its Pod. Start with a copy or overlay of the template. Configure your cluster's device plugin to advertise the serial gateway as an extended resource. Match the resource name under `resources.limits`, set `CHAINS_SETTINGS__PORT` to the path the plugin exposes, and set `supplementalGroups` to its numeric device-access group. Confirm that the selected node advertises the resource before applying the template in the same namespace as `chains-runtime` with `kubectl -n YOUR_NAMESPACE apply -k YOUR_RFLINK_OVERLAY`. A node label only controls placement. [Node selectors](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#nodeselector) · [Device plugins](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/)

You can also run `chains-service rflink` or `chains-go-service keyboard` directly on the Linux machine attached to the hardware and connect to the same broker. Use a stable device path and grant the host service account access to that device. See [RFLink setup](services-python.md#rflink-usb) or [keyboard setup](sdk-go.md#bundled-usb-keyboard).
