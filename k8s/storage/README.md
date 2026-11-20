# storage

**TL;DR:** `premium-zrs` is the default StorageClass: Azure Disk CSI, `Premium_ZRS`, `WaitForFirstConsumer`, volume expansion, platform-managed keys. AKS's `default` class is redeclared here with its default flag set to `"false"`. This sticks because AKS creates its classes with `EnsureExists`. `csi-azuredisk-vsc` is the VolumeSnapshotClass, with incremental snapshots. AKS ships none. `make storage` applies it each morning after the node pools. ArgoCD takes this over in P7.

Filled by P4-01 (#48) and P4-02 (#49). The Azure Disk CSI driver and the snapshot controller are AKS add-ons, enabled in the [aks module](../../infra/modules/aks/README.md#add-ons) (P3-09).

## Resources

| File | Kind | Name | Purpose |
| --- | --- | --- | --- |
| `storageclass-premium-zrs.yaml` | `StorageClass` | `premium-zrs` | The default. Premium SSD, zone-redundant, for Postgres and Redis |
| `storageclass-default.yaml` | `StorageClass` | `default` | AKS's own default class, redeclared with `is-default-class: "false"` |
| `volumesnapshotclass.yaml` | `VolumeSnapshotClass` | `csi-azuredisk-vsc` | Incremental snapshots of `disk.csi.azure.com` volumes. The default snapshot class |

## The StorageClass

| Setting | Value | Why |
| --- | --- | --- |
| `skuName` | `Premium_ZRS` | Premium SSD, as the brief asks. ZRS lets a disk attach in any zone, so Postgres can move zones. See [Postgres](../../docs/architecture-decisions.md#postgres) |
| `volumeBindingMode` | `WaitForFirstConsumer` | The disk is created once the pod is scheduled. ZRS doesn't need this for zones, but it stops PVCs provisioning disks nothing uses |
| `allowVolumeExpansion` | `true` | A PVC can grow without recreating the StatefulSet |
| `reclaimPolicy` | `Delete` | The disks live in the node resource group, which goes with the cluster each evening. `Retain` would keep nothing past that |
| Encryption | Platform-managed keys | No `diskEncryptionSetID`. See [Disk encryption](../../docs/architecture-decisions.md#disk-encryption) |

AKS also ships `managed-csi-premium` with the same SKU and settings. We keep our own class so the name and spec are in Git, since AKS's defaults change between versions.

## How AKS handles its built-in classes

AKS creates six classes on a new cluster, all on `disk.csi.azure.com`. In a region with zones they are ZRS:

| Class | SKU | Default |
| --- | --- | --- |
| `default` | `StandardSSD_ZRS` | Yes |
| `managed-csi` | `StandardSSD_ZRS` | No |
| `managed` | `StandardSSD_ZRS` | No |
| `managed-csi-premium` | `Premium_ZRS` | No |
| `managed-premium` | `Premium_ZRS` | No |
| `managed-csi-premium-v2` | `PremiumV2_LRS` | No |

`default` carries the flag. `managed-csi` has the same parameters but no flag. Every built-in class has the label `addonmanager.kubernetes.io/mode: EnsureExists`. The addon manager creates an `EnsureExists` object only when it's missing and leaves edits alone. It reverts a `Reconcile` object within a minute. So:

- Setting the annotation to `"false"` on `default` sticks for the life of the cluster.
- Deleting `default` doesn't work. The addon manager recreates it with the flag set.

The cluster is new every morning, so a manifest makes the change, not a one-off `kubectl annotate`. `storageclass-default.yaml` repeats AKS's spec for `default` with the flag set to `"false"`. The spec must match the live class, because StorageClass parameters can't change. If an AKS release changes them, the apply fails with `Forbidden: updates to parameters are forbidden`. Then copy the new values from `kubectl get sc default -o yaml`.

Because this folder declares `default`, deleting the folder's resources deletes AKS's class too, and the addon manager brings it back as the default. So:

- Never run `kubectl delete -k k8s/storage`, the cleanup pattern in [node-pools](../node-pools/README.md#acceptance-test). Delete test resources by name, as in the [acceptance test](#acceptance-test).
- `storageclass-default.yaml` carries `argocd.argoproj.io/sync-options: Prune=false,Delete=false`, so from P7 neither an ArgoCD prune nor deleting the Application removes it.

To check the label is still `EnsureExists` on a new cluster:

```bash
kubectl get sc -L addonmanager.kubernetes.io/mode
```

If AKS switches it to `Reconcile`, the flag returns within a minute. Kubernetes then has two defaults and gives new PVCs the newest, which is `premium-zrs`. The Postgres and Redis StatefulSets (P5) should name `storageClassName: premium-zrs`, so they never depend on the default.

## The VolumeSnapshotClass

AKS installs the snapshot CRDs and the snapshot controller, but no VolumeSnapshotClass. A new cluster lists none with `kubectl get volumesnapshotclass`, so there's no `csi-azuredisk-vsc` to reuse. Ours uses the name from the [AKS](https://learn.microsoft.com/en-us/azure/aks/azure-disk-csi#volume-snapshots) and [Azure Disk CSI](https://github.com/kubernetes-sigs/azuredisk-csi-driver/tree/master/deploy/example/snapshot) docs, so their examples work as written.

| Setting | Value | Why |
| --- | --- | --- |
| `incremental` | `"true"` | Each snapshot stores only the blocks changed since the last one, billed at about $0.053 per GB a month. The driver's default is already `true`. Set here so it shows |
| `deletionPolicy` | `Delete` | Deleting the VolumeSnapshot deletes the Azure snapshot |
| Default | `snapshot.storage.kubernetes.io/is-default-class: "true"` | A VolumeSnapshot with no class uses this one |

Snapshots go to the node resource group, `rg-aks-v2-platform-nodes`, and are deleted with it each evening. They serve the snapshot and restore test within a day. They aren't a backup that outlives the cluster. For that, set a `resourceGroup` parameter pointing at a group that survives, with a role for the cluster identity there, or use Velero (see the brief's Advanced section).

## Morning apply

After the platform apply, `kubectl apply -k k8s/node-pools` and `kubectl apply -k k8s/namespaces`:

```bash
kubectl apply -k k8s/storage
```

The first apply to `default` prints a warning about a missing `last-applied-configuration` annotation. This is expected. AKS created the class, and kubectl adds the annotation.

## Acceptance test

This needs a live cluster with the NodePools applied, so the test pod lands on a NAP node. Run from the repo root.

Preview the change first. Only the annotation on `default` and the two new objects should differ:

```bash
kubectl diff -k k8s/storage
kubectl apply -k k8s/storage
```

P4-01 (#48): only `premium-zrs` is the default.

```bash
kubectl get sc
```

`premium-zrs` should be the only class marked `(default)`. Then check a PVC with no class gets a Premium ZRS disk:

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: storage-test
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 4Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: storage-test
spec:
  containers:
    - name: test
      image: mcr.microsoft.com/azurelinux/base/core:3.0
      command: ["sh", "-c", "echo hello > /data/hello && sleep 3600"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: storage-test
EOF
kubectl wait --for=condition=Ready pod/storage-test --timeout=5m
kubectl get pvc storage-test -o jsonpath='{.spec.storageClassName}{"\n"}'
kubectl get pv "$(kubectl get pvc storage-test -o jsonpath='{.spec.volumeName}')" \
  -o jsonpath='{.spec.csi.volumeAttributes.skuName}{"\n"}'
```

Expect `premium-zrs`, then `Premium_ZRS`.

P4-02 (#49): a snapshot of that PVC reaches `readyToUse: true`.

```bash
kubectl apply -f - <<'EOF'
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: storage-test
spec:
  source:
    persistentVolumeClaimName: storage-test
EOF
kubectl wait --for=jsonpath='{.status.readyToUse}'=true volumesnapshot/storage-test --timeout=5m
kubectl get volumesnapshot storage-test
```

`READYTOUSE` should say `true`. `SNAPSHOTCLASS` should say `csi-azuredisk-vsc`, since the snapshot named no class.

Clean up the test resources by name. Deleting the PVC deletes the disk, and deleting the VolumeSnapshot deletes the Azure snapshot. Leave the classes in place, and don't use `kubectl delete -k k8s/storage`:

```bash
kubectl delete volumesnapshot storage-test
kubectl delete pod storage-test
kubectl delete pvc storage-test
```

The full restore drill, a new PVC from the snapshot with the Postgres data intact, belongs to the Postgres card in P5.

## Validate

```bash
kustomize build k8s/storage | kubeconform -strict -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
```
