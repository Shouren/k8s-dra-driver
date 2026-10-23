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
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	yamlv3 "go.yaml.in/yaml/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func renderMemoryChart(t *testing.T, values string) (*corev1.ConfigMap, *appsv1.DaemonSet) {
	t.Helper()
	return renderMemoryChartForRelease(t, "memory-test", values)
}

func renderMemoryChartForRelease(t *testing.T, release, values string) (*corev1.ConfigMap, *appsv1.DaemonSet) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	args := []string{"template", release, "../../chart/hami-dra-driver", "--namespace", "hami-system", "--kube-version", "1.34.0", "--api-versions", "resource.k8s.io/v1", "-f", writeMemoryPolicy(t, values)}
	output, err := exec.Command("helm", args...).CombinedOutput()
	require.NoError(t, err, string(output))
	decoder := yamlv3.NewDecoder(bytes.NewReader(output))
	var cm *corev1.ConfigMap
	var ds *appsv1.DaemonSet
	for {
		var document yamlv3.Node
		err := decoder.Decode(&document)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		data, err := yamlv3.Marshal(&document)
		require.NoError(t, err)
		data, err = yaml.YAMLToJSON(data)
		require.NoError(t, err)
		var meta struct {
			Kind string `json:"kind"`
		}
		require.NoError(t, json.Unmarshal(data, &meta))
		switch meta.Kind {
		case "ConfigMap":
			var candidate corev1.ConfigMap
			require.NoError(t, json.Unmarshal(data, &candidate))
			if _, ok := candidate.Data["device-memory.yaml"]; ok {
				cm = &candidate
			}
		case "DaemonSet":
			ds = &appsv1.DaemonSet{}
			require.NoError(t, json.Unmarshal(data, ds))
		}
	}
	require.NotNil(t, ds)
	return cm, ds
}

func TestDeviceMemoryChartOrderAndWiring(t *testing.T) {
	// Deliberately reverse alphabetical order; both rules match the same node.
	values := `driver:
  preConfiguredDeviceMemory: "0"
  nodeConfig:
  - name: z-first
    nodeSelector: {matchLabels: {pool: spark}}
    preConfiguredDeviceMemory: "1.1Mi"
  - name: a-second
    nodeSelector: {matchLabels: {pool: spark}}
    preConfiguredDeviceMemory: "64Gi"
`
	cm, ds := renderMemoryChart(t, values)
	require.NotNil(t, cm)
	policy, err := parseDeviceMemoryPolicy([]byte(cm.Data["device-memory.yaml"]))
	require.NoError(t, err)
	budget, shadowed := policy.resolve(map[string]string{"pool": "spark"})
	require.Equal(t, "z-first", budget.source)
	require.Equal(t, "1.1Mi", budget.requested)
	require.Equal(t, uint64(2097152), budget.bytes)
	require.Equal(t, []string{"a-second"}, shadowed)
	checksum := ds.Spec.Template.Annotations["checksum/device-memory"]
	require.NotEmpty(t, checksum)
	_, same := renderMemoryChart(t, values)
	require.Equal(t, checksum, same.Spec.Template.Annotations["checksum/device-memory"])
	foundEnv, foundMount, foundVolume := false, false, false
	for _, container := range ds.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == "DEVICE_MEMORY_CONFIG_FILE" {
				require.Equal(t, "/etc/hami-dra/device-memory.yaml", env.Value)
				foundEnv = true
			}
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == "device-memory" {
				require.True(t, mount.ReadOnly)
				require.Equal(t, "/etc/hami-dra", mount.MountPath)
				foundMount = true
			}
		}
	}
	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Name == "device-memory" {
			require.NotNil(t, volume.ConfigMap)
			require.Equal(t, cm.Name, volume.ConfigMap.Name)
			foundVolume = true
		}
	}
	require.True(t, foundEnv && foundMount && foundVolume)
	reversed := `driver:
  preConfiguredDeviceMemory: "0"
  nodeConfig:
  - name: a-second
    nodeSelector: {matchLabels: {pool: spark}}
    preConfiguredDeviceMemory: "64Gi"
  - name: z-first
    nodeSelector: {matchLabels: {pool: spark}}
    preConfiguredDeviceMemory: "1.1Mi"
`
	cm, ds = renderMemoryChart(t, reversed)
	require.NotEqual(t, checksum, ds.Spec.Template.Annotations["checksum/device-memory"])
	policy, err = parseDeviceMemoryPolicy([]byte(cm.Data["device-memory.yaml"]))
	require.NoError(t, err)
	budget, shadowed = policy.resolve(map[string]string{"pool": "spark"})
	require.Equal(t, "a-second", budget.source)
	require.Equal(t, uint64(68719476736), budget.bytes)
	require.Equal(t, []string{"z-first"}, shadowed)
}

func TestDeviceMemoryChartDisabled(t *testing.T) {
	// Even a leftover invalid budget is ignored while the feature is off.
	cm, ds := renderMemoryChart(t, "featureGates:\n  HAMiCoreSupport: false\ndriver:\n  preConfiguredDeviceMemory: 123\n")
	require.Nil(t, cm)
	require.NotContains(t, ds.Spec.Template.Annotations, "checksum/device-memory")
	for _, container := range ds.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			require.NotEqual(t, "DEVICE_MEMORY_CONFIG_FILE", env.Name)
		}
		for _, mount := range container.VolumeMounts {
			require.NotEqual(t, "device-memory", mount.Name)
		}
	}
	for _, volume := range ds.Spec.Template.Spec.Volumes {
		require.NotEqual(t, "device-memory", volume.Name)
	}
}

func TestDeviceMemoryChartRejectsInvalidStructure(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart validation tests")
	}
	for _, values := range []string{
		"driver:\n  preConfiguredDeviceMemory: 64\n",
		"driver:\n  nodeConfig: {named: {}}\n",
		"driver:\n  nodeConfig: [{name: test, nodeSelector: {matchLabels: {pool: spark}}}]\n",
	} {
		out, err := exec.Command("helm", "template", "memory-test", "../../chart/hami-dra-driver", "--namespace", "hami-system", "--kube-version", "1.34.0", "--api-versions", "resource.k8s.io/v1", "-f", writeMemoryPolicy(t, values)).CombinedOutput()
		require.Error(t, err, string(out))
		require.Contains(t, string(out), "driver.")
	}
}

func TestDeviceMemoryConfigMapReleaseName(t *testing.T) {
	var names []string
	for _, release := range []string{"memory-first", "memory-second"} {
		cm, ds := renderMemoryChartForRelease(t, release, "{}")
		require.NotNil(t, cm)
		require.Equal(t, release+"-device-memory", cm.Name)
		for _, volume := range ds.Spec.Template.Spec.Volumes {
			if volume.Name == "device-memory" {
				require.NotNil(t, volume.ConfigMap)
				require.Equal(t, cm.Name, volume.ConfigMap.Name)
			}
		}
		names = append(names, cm.Name)
	}
	require.NotEqual(t, names[0], names[1])
}

func TestDeviceMemoryConfigMapLongReleaseName(t *testing.T) {
	release := strings.Repeat("a", 53)
	cm, ds := renderMemoryChartForRelease(t, release, "{}")
	require.NotNil(t, cm)
	require.LessOrEqual(t, len(cm.Name), 63)
	require.True(t, strings.HasPrefix(cm.Name, release))
	for _, volume := range ds.Spec.Template.Spec.Volumes {
		if volume.Name == "device-memory" {
			require.NotNil(t, volume.ConfigMap)
			require.Equal(t, cm.Name, volume.ConfigMap.Name)
			return
		}
	}
	t.Fatal("device memory ConfigMap volume is missing")
}
