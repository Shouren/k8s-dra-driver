# HAMi DRA Driver

## Overview

DRA is a Kubernetes feature that lets you request and share resources among Pods, it provides a flexible way for requesting, configuring, and sharing specialized devices like GPUs. To learn more about DRA in general, good starting points are: [Kubernetes docs](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/), [Kubernetes blog](https://kubernetes.io/blog/2025/05/01/kubernetes-v1-33-dra-updates/).

The HAMi DRA Driver brings the support of DRA for [HAMi-Core](https://github.com/Project-HAMi/HAMi-core)(the in-container GPU resource controller), it might be the first open source DRA Driver for NVIDIA GPU with [DRA Consumable Capacity](https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5075-dra-consumable-capacity) feature enabled.

## Installation

### Prerequisites

The list of prerequisites for running the DRA Driver for HAMi-Core is described below
- NVIDIA drivers >= 440
- nvidia-docker version > 2.0
- default runtime configured as nvidia for containerd/docker/cri-o container runtime
  - `accept-nvidia-visible-devices-as-volume-mounts` option should be set to `true` for NVIDIA Container Runtime
- Kubernetes version >= 1.34 with DRA Consumable Capacity feature enabled
- glibc >= 2.17 & glibc < 2.30
- kernel version >= 3.10


Please check the [DaemonSet configuration](demo/yaml/ds.yaml) in the demo/yaml directory. We are working on a Helm Chart for this DRA Driver, it will be available in the near future.

## Memory fallback for unified-memory GPUs

With `HAMiCoreSupport=true` (the default), GPUs such as GB10 can use an
operator-configured memory budget when NVML returns `ERROR_NOT_SUPPORTED`.
Successful NVML memory reporting always wins. Without a configured budget,
unsupported GPUs are skipped with a warning; other NVML failures remain errors.
With HAMi-Core disabled, the inherited discovery behavior is unchanged.

Install using the [example Helm values](demo/yaml/gb10-values.yaml):

```bash
helm upgrade --install hami-dra-driver ./chart/hami-dra-driver \
  --namespace hami-system --create-namespace -f demo/yaml/gb10-values.yaml
```

Configure node labels through your node pool or provisioning automation. Rules
use Kubernetes `LabelSelector` (`matchLabels` or `matchExpressions`); the first
matching entry in `driver.nodeConfig` wins. No match uses
`driver.preConfiguredDeviceMemory`, defaulting to `"0"` (disabled). Each rule
requires a unique name, nonempty selector, and a memory Quantity string.

Use Kubernetes Quantity strings such as `"64Gi"` or `"65536Mi"`. The supported
precision is **1 MiB**: positive values round **up** to a whole MiB, for example
`"1.1Mi"` becomes `"2Mi"`. Unitless values are bytes, and `m` means milli, not
MiB. Startup logs report requested and effective capacity and warn on rounding.
Budget for the effective value and reserve shared system RAM for the OS and CPU
workloads. This is a per-GPU scheduling budget, not a physical RAM reservation.

Configuration and node labels are read once at startup. The default chart
already grants `nodes/get`; custom RBAC must grant it when using node rules.
The memory policy ConfigMap name is qualified by the Helm release name; other
chart resources retain their existing naming convention.
Changing Helm configuration triggers a rollout through a checksum annotation;
label changes or direct ConfigMap edits require an explicit plugin rollout.
Retire affected GPU workloads and release their claims before changing budgets.
The existing single-GPU claim and Pod in `demo/yaml/setup.yaml` and
`demo/yaml/pod-0.yaml` can be used for subsequent hardware validation. GB10
runtime compatibility and enforcement still require testing on actual hardware.

For non-Helm deployments, pass `--device-memory-config-file` or
`DEVICE_MEMORY_CONFIG_FILE` pointing to the same policy without the outer
`driver` key. Disabled HAMi-Core ignores this file. No configured file means a
zero fallback with no node rules.

## Demo

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
...
spec:
  devices:
    requests:
    - name: double-gpu-0-0
      exactly:
        ...
        capacity:
          requests:
            cores: 30
            memory: "4Gi"
    - name: double-gpu-0-1
      exactly:
        ...
        capacity:
          requests:
            cores: 60
            memory: "8Gi"
```
![img](./imgs/hard_limit.jpg)

Please refer to the [demo.md](demo/demo.md) for more information.

## Contributing

Contributions require a Developer Certificate of Origin (DCO, see [CONTRIBUTING.md](https://github.com/Project-HAMi/HAMi/blob/master/CONTRIBUTING.md).

## Support

Please open an issue on the GitHub project for questions and for reporting problems.
Your feedback is appreciated!
