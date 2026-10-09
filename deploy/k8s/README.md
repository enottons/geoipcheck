# Kubernetes

One CronJob downloads GeoLite2-Country onto a ReadWriteMany volume. Every
geoipcheck pod mounts that volume read-only and hot-reloads the file when it
changes ([design.md](../../docs/design.md) D8, D11). Alerts and the runbook
are in [operations.md](../../docs/operations.md).

These manifests are checked with `kustomize build | kubeconform` in CI. They
have not been applied to a live cluster as part of this submission. The one
thing I'd check first on a real cluster is the geoipupdate image under
`readOnlyRootFilesystem`: only `/data` and `/tmp` are writable. If its
entrypoint writes anywhere else, add an `emptyDir` there.

## Install

1. Pick an RWX storage class and set `storageClassName` in `pvc.yaml`. The
   volume root must be writable by uid 65532. Most RWX drivers ignore
   `fsGroup`, so set this on the export (NFS `anonuid`/ownership, an EFS
   access point with uid 65532).

2. Create the license secret. It is not in the kustomization so the key
   never lands in git:

   ```sh
   kubectl create namespace geoipcheck
   kubectl -n geoipcheck create secret generic geoipupdate \
     --from-literal=GEOIPUPDATE_ACCOUNT_ID=<id> \
     --from-literal=GEOIPUPDATE_LICENSE_KEY=<key>
   ```

3. Apply, then seed the volume instead of waiting for the first schedule:

   ```sh
   kubectl apply -k deploy/k8s
   kubectl -n geoipcheck create job geoipupdate-seed --from=cronjob/geoipupdate
   kubectl -n geoipcheck wait --for=condition=complete job/geoipupdate-seed --timeout=5m
   ```

   Pods start before the seed job finishes. They stay not ready, out of the
   Service, until the file appears, then go ready within one poll (60s).

4. Label the gateway's namespace so the NetworkPolicy lets it in:

   ```sh
   kubectl label namespace <gateway-ns> geoipcheck.enottons.github.io/client=true
   ```

## Shutdown budget

`terminationGracePeriodSeconds` (30) must exceed `GEOIPCHECK_DRAIN_DELAY`
(5s) plus `GEOIPCHECK_SHUTDOWN_TIMEOUT` (15s) with margin. Change them
together.

## No RWX storage

Run geoipupdate as a sidecar in each pod with `GEOIPUPDATE_FREQUENCY=24`,
writing to a shared `emptyDir`. Each replica then downloads on its own, which
multiplies license quota use by the replica count. That's why it isn't the
default.
