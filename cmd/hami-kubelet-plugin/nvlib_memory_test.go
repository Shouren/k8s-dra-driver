/*
 * Copyright 2026 The HAMi Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	nvmlmock "github.com/NVIDIA/go-nvml/pkg/nvml/mock"
	"github.com/Project-HAMi/k8s-dra-driver/pkg/featuregates"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
)

// Embed the hardware interface so unexpected calls fail, instead of silently
// succeeding. Only discovery calls used by this full-GPU fixture are simulated.
type memoryTestDevice struct {
	nvdev.Device
	index      int
	status     nvml.Return
	total      uint64
	mig        bool
	nameStatus nvml.Return
}

func (d memoryTestDevice) GetMinorNumber() (int, nvml.Return) { return d.index, nvml.SUCCESS }
func (d memoryTestDevice) GetUUID() (string, nvml.Return) {
	return fmt.Sprintf("GPU-test-%d", d.index), nvml.SUCCESS
}
func (d memoryTestDevice) IsMigEnabled() (bool, error) { return d.mig, nil }
func (d memoryTestDevice) GetMemoryInfo() (nvml.Memory, nvml.Return) {
	return nvml.Memory{Total: d.total}, d.status
}
func (d memoryTestDevice) GetName() (string, nvml.Return)                    { return "test GPU", d.nameStatus }
func (d memoryTestDevice) GetArchitectureAsString() (string, error)          { return "Blackwell", nil }
func (d memoryTestDevice) GetBrandAsString() (string, error)                 { return "NVIDIA", nil }
func (d memoryTestDevice) GetCudaComputeCapabilityAsString() (string, error) { return "12.1", nil }
func (d memoryTestDevice) GetPCIBusID() (string, error)                      { return "0000:ff:00.0", nil }
func (d memoryTestDevice) GetAddressingModeAsString() (string, error)        { return "", nil }
func (d memoryTestDevice) GetGpuInstanceProfileInfo(int) (nvml.GpuInstanceProfileInfo, nvml.Return) {
	return nvml.GpuInstanceProfileInfo{}, nvml.ERROR_NOT_SUPPORTED
}

type memoryTestDevices struct {
	nvdev.Interface
	devices []nvdev.Device
}

func (d memoryTestDevices) VisitDevices(visit func(int, nvdev.Device) error) error {
	for i, device := range d.devices {
		if err := visit(i, device); err != nil {
			return err
		}
	}
	return nil
}

func memoryTestLibrary(devices ...nvdev.Device) *deviceLib {
	return &deviceLib{
		Interface: memoryTestDevices{devices: devices},
		nvmllib: &nvmlmock.Interface{
			InitFunc:                       func() nvml.Return { return nvml.SUCCESS },
			ShutdownFunc:                   func() nvml.Return { return nvml.SUCCESS },
			SystemGetDriverVersionFunc:     func() (string, nvml.Return) { return "580.95.05", nvml.SUCCESS },
			SystemGetCudaDriverVersionFunc: func() (int, nvml.Return) { return 13000, nvml.SUCCESS },
		},
		gpuInfosByUUID: make(map[string]*GpuInfo), gpuUUIDbyMinor: make(map[GPUMinor]string), devhandleByUUID: make(map[string]nvml.Device),
	}
}

func TestGPUMemoryDiscovery(t *testing.T) {
	for _, tt := range []struct {
		name                string
		enabled             bool
		status              nvml.Return
		total, budget, want uint64
		mig, skip, fail     bool
	}{
		{name: "NVML wins", enabled: true, status: nvml.SUCCESS, total: 8589934592, budget: 68719476736, want: 8589934592},
		{name: "successful zero unchanged", enabled: true, status: nvml.SUCCESS, total: 0, budget: 68719476736},
		{name: "configured fallback", enabled: true, status: nvml.ERROR_NOT_SUPPORTED, budget: 68719476736, want: 68719476736},
		{name: "unconfigured skipped", enabled: true, status: nvml.ERROR_NOT_SUPPORTED, skip: true},
		{name: "MIG cannot use fallback", enabled: true, status: nvml.ERROR_NOT_SUPPORTED, budget: 68719476736, mig: true, skip: true},
		{name: "disabled preserves unsupported error", status: nvml.ERROR_NOT_SUPPORTED, budget: 68719476736, fail: true},
		{name: "disabled success unchanged", status: nvml.SUCCESS, total: 8589934592, budget: 68719476736, want: 8589934592},
		{name: "GPU lost remains fatal", enabled: true, status: nvml.ERROR_GPU_IS_LOST, budget: 68719476736, fail: true},
		{name: "permission error remains fatal", enabled: true, status: nvml.ERROR_NO_PERMISSION, budget: 68719476736, fail: true},
		{name: "unknown error remains fatal", enabled: true, status: nvml.ERROR_UNKNOWN, budget: 68719476736, fail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, featuregates.FeatureGates(), featuregates.HAMiCoreSupport, tt.enabled)
			l := memoryTestLibrary()
			l.deviceMemoryBytes = tt.budget
			gpu, err := l.getGpuInfo(0, memoryTestDevice{status: tt.status, total: tt.total, mig: tt.mig})
			if tt.skip {
				require.ErrorIs(t, err, errDeviceMemoryUnavailable)
				require.Nil(t, gpu)
				return
			}
			if tt.fail {
				require.ErrorIs(t, err, tt.status)
				require.NotErrorIs(t, err, errDeviceMemoryUnavailable)
				require.Nil(t, gpu)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, gpu.memoryBytes)
		})
	}
}

func TestGPUMemoryEnumeration(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, featuregates.FeatureGates(), featuregates.HAMiCoreSupport, true)
	l := memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED}, memoryTestDevice{index: 1, status: nvml.SUCCESS, total: 8589934592})
	all, perGPU, err := l.enumerateAllPossibleDevices()
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Contains(t, all, "hami-gpu-1")
	require.Len(t, perGPU, 1)
	require.Contains(t, perGPU, 1)
	require.NotContains(t, l.gpuInfosByUUID, "GPU-test-0")
	require.NotContains(t, l.gpuUUIDbyMinor, 0)
	require.Empty(t, l.devhandleByUUID)
	l = memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED})
	all, perGPU, err = l.enumerateAllPossibleDevices()
	require.NoError(t, err)
	require.Empty(t, all)
	require.Empty(t, perGPU)
	require.Empty(t, l.gpuInfosByUUID)
	l = memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED, nameStatus: nvml.ERROR_UNKNOWN})
	l.deviceMemoryBytes = 68719476736
	_, _, err = l.enumerateAllPossibleDevices()
	require.Error(t, err)
	require.NotErrorIs(t, err, errDeviceMemoryUnavailable)
}

func TestHAMiFallbackCapacity(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, featuregates.FeatureGates(), featuregates.HAMiCoreSupport, true)
	for _, tt := range []struct {
		raw   string
		bytes int64
	}{{"64Gi", 68719476736}, {"1.1Mi", 2097152}} {
		t.Run(tt.raw, func(t *testing.T) {
			policy, err := parseDeviceMemoryPolicy([]byte(fmt.Sprintf("preConfiguredDeviceMemory: %q", tt.raw)))
			require.NoError(t, err)
			budget, _ := policy.resolve(nil)
			l := memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED})
			l.deviceMemoryBytes = budget.bytes
			all, _, err := l.enumerateAllPossibleDevices()
			require.NoError(t, err)
			dev := all["hami-gpu-0"]
			// The optional PCIe-root nil guard is a separately tracked fix. Supply this
			// attribute explicitly to exercise capacity publication without that issue.
			dev.HAMiGpu.pcieRootAttr = &deviceattribute.DeviceAttribute{Name: "testRoot", Value: resourceapi.DeviceAttribute{StringValue: ptr.To("root")}}
			published := dev.GetDevice()
			cap := published.Capacity["memory"]
			require.Equal(t, tt.bytes, cap.Value.Value())
			require.Equal(t, tt.bytes, cap.RequestPolicy.Default.Value())
			require.Equal(t, tt.bytes, cap.RequestPolicy.ValidRange.Max.Value())
			require.Equal(t, int64(1048576), cap.RequestPolicy.ValidRange.Min.Value())
			require.Equal(t, int64(1048576), cap.RequestPolicy.ValidRange.Step.Value())
		})
	}
}

func newMemoryTestManager(t *testing.T) *HAMiCoreManager {
	t.Helper()
	manager := NewHAMiCoreManager(nil)
	manager.hostHookPath = t.TempDir()
	manager.claimCacheRoot = filepath.Join(manager.hostHookPath, "vgpu", "claims")
	return manager
}

func TestHAMiFallbackClaimMemoryLimit(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, featuregates.FeatureGates(), featuregates.HAMiCoreSupport, true)
	lib := memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED})
	lib.deviceMemoryBytes = 68719476736
	devices, _, err := lib.enumerateAllPossibleDevices()
	require.NoError(t, err)
	require.Len(t, devices, 1)
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: testClaimUID},
		Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{
			Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{{
				Device: "hami-gpu-0",
				ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{
					"memory": resource.MustParse("8Gi"),
					"cores":  resource.MustParse("30"),
				},
			}}},
		}},
	}
	edits := newMemoryTestManager(t).GetCDIContainerEdits(claim, devices)
	require.Contains(t, edits.Env, "CUDA_DEVICE_MEMORY_LIMIT_0=8192m")
	require.NotContains(t, edits.Env, "CUDA_DEVICE_MEMORY_LIMIT_0=65536m")
	require.Contains(t, edits.Env, "CUDA_DEVICE_SM_LIMIT_0=30")
}

func TestUnprepareCheckpointedClaimAfterGPUSkipped(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, featuregates.FeatureGates(), featuregates.HAMiCoreSupport, true)
	ctx := context.Background()
	lib := memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED})
	lib.deviceMemoryBytes = 68719476736
	devices, _, err := lib.enumerateAllPossibleDevices()
	require.NoError(t, err)
	manager := newMemoryTestManager(t)
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: testClaimUID, Name: "claim", Namespace: "test"},
		Status:     resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{}},
	}
	edits := manager.GetCDIContainerEdits(claim, devices)
	cacheDir := edits.Mounts[0].HostPath
	require.DirExists(t, cacheDir)
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "allocation.cache"), []byte("prepared"), 0600))

	state := newTestDeviceState(t)
	state.hamiCoreManager = manager
	state.cdi = &CDIHandler{cdiRoot: t.TempDir()}
	specPath := filepath.Join(state.cdi.cdiRoot, cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, testClaimUID)+".yaml")
	require.NoError(t, os.WriteFile(specPath, []byte("cdiVersion: 0.6.0\n"), 0600))
	prepared := completedHAMiClaim()
	prepared.PreparedDevices[0].Devices[0].HAMiGpu.Info = devices["hami-gpu-0"].HAMiGpu
	prepared.PreparedDevices[0].Devices[0].HAMiGpu.Device.DeviceName = "hami-gpu-0"
	require.NoError(t, state.createCheckpoint(ctx, &Checkpoint{V2: &CheckpointV2{
		NodeBootID:     testBootID,
		PreparedClaims: PreparedClaimsByUID{testClaimUID: prepared},
	}}))

	// Simulate rediscovery after restart without the fallback budget. Discovery
	// skips the GPU, but must not erase the existing preparation or cache.
	skippedLib := memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED})
	state.allocatable, state.perGPUAllocatable, err = skippedLib.enumerateAllPossibleDevices()
	require.NoError(t, err)
	require.Empty(t, state.allocatable)
	checkpoint, err := state.getCheckpoint(ctx)
	require.NoError(t, err)
	require.Contains(t, checkpoint.V2.PreparedClaims, testClaimUID)
	require.DirExists(t, cacheDir)
	require.FileExists(t, specPath)

	ref := kubeletplugin.NamespacedObject{UID: claim.UID, NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}
	require.NoError(t, state.Unprepare(ctx, ref))
	require.NoDirExists(t, cacheDir)
	require.NoFileExists(t, specPath)
	checkpoint, err = state.getCheckpoint(ctx)
	require.NoError(t, err)
	require.NotContains(t, checkpoint.V2.PreparedClaims, testClaimUID)
	require.NoError(t, state.Unprepare(ctx, ref))
}

func TestPublishResourcesRemovesPreviouslyAdvertisedSkippedGPU(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, featuregates.FeatureGates(), featuregates.HAMiCoreSupport, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lib := memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED})
	lib.deviceMemoryBytes = 68719476736
	devices, _, err := lib.enumerateAllPossibleDevices()
	require.NoError(t, err)
	devices["hami-gpu-0"].HAMiGpu.pcieRootAttr = &deviceattribute.DeviceAttribute{
		Name: "testRoot", Value: resourceapi.DeviceAttribute{StringValue: ptr.To("root")},
	}
	client := fake.NewClientset()
	// The fake API does not implement server-side GenerateName. Supply the
	// generated name before allowing its ordinary object tracker to handle create.
	client.PrependReactor("create", "resourceslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		slice := action.(clienttesting.CreateAction).GetObject().(*resourceapi.ResourceSlice)
		slice.Name = "published-memory-test"
		slice.ResourceVersion = "1"
		return false, nil, nil
	})
	d := &driver{client: client, state: &DeviceState{allocatable: devices}}
	helper, err := kubeletplugin.Start(ctx, d,
		kubeletplugin.KubeClient(client), kubeletplugin.DriverName(DriverName),
		kubeletplugin.NodeName("node"), kubeletplugin.NodeUID("node-uid"),
		kubeletplugin.DRAService(false), kubeletplugin.RegistrationService(false),
	)
	require.NoError(t, err)
	defer helper.Stop()
	d.pluginhelper = helper
	config := &Config{flags: &Flags{nodeName: "node"}}
	require.NoError(t, d.publishResources(ctx, config))
	require.Eventually(t, func() bool {
		slices, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
		return err == nil && len(slices.Items) == 1 && len(slices.Items[0].Spec.Devices) == 1 && slices.Items[0].Spec.Devices[0].Name == "hami-gpu-0"
	}, 5*time.Second, 10*time.Millisecond, "initial GPU was not published")

	// Feed actual empty discovery into the same publishing path and controller.
	lib = memoryTestLibrary(memoryTestDevice{status: nvml.ERROR_NOT_SUPPORTED})
	d.state.allocatable, d.state.perGPUAllocatable, err = lib.enumerateAllPossibleDevices()
	require.NoError(t, err)
	require.Empty(t, d.state.allocatable)
	require.NoError(t, d.publishResources(ctx, config))
	require.Eventually(t, func() bool {
		slices, err := client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
		return err == nil && len(slices.Items) == 1 && len(slices.Items[0].Spec.Devices) == 0 && slices.Items[0].Spec.Driver == DriverName && slices.Items[0].Spec.Pool.Name == "node"
	}, 5*time.Second, 10*time.Millisecond, "previously advertised GPU remained in ResourceSlices")
}
