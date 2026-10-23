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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pkgflags "github.com/Project-HAMi/k8s-dra-driver/pkg/flags"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"
)

func TestDeviceMemoryQuantity(t *testing.T) {
	for _, tt := range []struct {
		raw     string
		want    uint64
		invalid bool
	}{
		{raw: "0"}, {raw: "64Gi", want: 68719476736}, {raw: "65536Mi", want: 68719476736},
		{raw: "1.5Gi", want: 1610612736}, {raw: "1.1Mi", want: 2097152},
		{raw: "64G", want: 64000884736}, {raw: "400m", want: 1048576},
		{raw: "65536", want: 1048576}, {raw: "1n", want: 1048576},
		{raw: "1048576", want: 1048576}, {raw: "1048576.001", want: 2097152},
		{raw: "9223372036853727232", want: 9223372036853727232},
		{raw: "9223372036853727233", invalid: true}, {raw: "9Ei", invalid: true},
		{raw: "-1Mi", invalid: true}, {raw: "64GB", invalid: true}, {raw: "", invalid: true},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			policy, err := parseDeviceMemoryPolicy([]byte(fmt.Sprintf("preConfiguredDeviceMemory: %q\n", tt.raw)))
			if tt.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			budget, _ := policy.resolve(nil)
			require.Equal(t, tt.want, budget.bytes)
			require.Equal(t, tt.raw, budget.requested)
		})
	}
}

const orderedMemoryPolicy = `preConfiguredDeviceMemory: "2Gi"
nodeConfig:
- name: z-specific
  nodeSelector:
    matchLabels:
      pool: spark
      workload: cpu-heavy
  preConfiguredDeviceMemory: "48Gi"
- name: a-general
  nodeSelector:
    matchExpressions:
    - key: pool
      operator: In
      values: [spark, spark-test]
  preConfiguredDeviceMemory: "64Gi"
`

func TestDeviceMemoryRuleOrder(t *testing.T) {
	for i := 0; i < 3; i++ {
		policy, err := parseDeviceMemoryPolicy([]byte(orderedMemoryPolicy))
		require.NoError(t, err)
		labels := map[string]string{"pool": "spark", "workload": "cpu-heavy"}
		budget, shadowed := policy.resolve(labels)
		require.Equal(t, "z-specific", budget.source)
		require.Equal(t, uint64(51539607552), budget.bytes)
		require.Equal(t, []string{"a-general"}, shadowed)
		policy.rules[0], policy.rules[1] = policy.rules[1], policy.rules[0]
		budget, shadowed = policy.resolve(labels)
		require.Equal(t, "a-general", budget.source)
		require.Equal(t, uint64(68719476736), budget.bytes)
		require.Equal(t, []string{"z-specific"}, shadowed)
	}
}

func TestDeviceMemoryMatching(t *testing.T) {
	policy, err := parseDeviceMemoryPolicy([]byte(orderedMemoryPolicy))
	require.NoError(t, err)
	for _, tt := range []struct {
		labels map[string]string
		source string
		bytes  uint64
	}{
		{labels: map[string]string{"pool": "spark-test"}, source: "a-general", bytes: 68719476736},
		{labels: nil, source: "default", bytes: 2147483648},
		{labels: map[string]string{"pool": "other"}, source: "default", bytes: 2147483648},
	} {
		budget, _ := policy.resolve(tt.labels)
		require.Equal(t, tt.source, budget.source)
		require.Equal(t, tt.bytes, budget.bytes)
	}
	policy, err = parseDeviceMemoryPolicy([]byte(strings.Replace(orderedMemoryPolicy, "\"48Gi\"", "\"0\"", 1)))
	require.NoError(t, err)
	budget, _ := policy.resolve(map[string]string{"pool": "spark", "workload": "cpu-heavy"})
	require.Zero(t, budget.bytes)
	policy, err = parseDeviceMemoryPolicy([]byte("{}"))
	require.NoError(t, err)
	budget, _ = policy.resolve(nil)
	require.Zero(t, budget.bytes)
}

func TestDeviceMemoryInvalidConfig(t *testing.T) {
	validRule := "nodeConfig:\n- name: test\n  nodeSelector:\n    matchLabels: {pool: spark}\n  preConfiguredDeviceMemory: \"1Gi\"\n"
	for _, data := range []string{
		"", "null", "[]", "{}\n---\n{}", "{}\n---\n", "preConfiguredDeviceMemory: 64", "preConfiguredDeviceMemory: true", "preConfiguredDeviceMemory: null", "nodeConfig: null",
		"preConfiguredDeviceMemory: \"1Gi\"\npreConfiguredDeviceMemory: \"2Gi\"",
		"unknown: true", "nodeConfig: {test: {}}",
		strings.Replace(validRule, "name: test", "nodeName: test", 1),
		strings.Replace(validRule, "name: test", "name: ''", 1),
		strings.Replace(validRule, "matchLabels: {pool: spark}", "matchLabels: {}", 1),
		strings.Replace(validRule, "matchLabels: {pool: spark}", "matchLabels: {'bad label': spark}", 1),
		strings.Replace(validRule, "matchLabels: {pool: spark}", "matchExpressions: [{key: pool, operator: Bad}]", 1),
		strings.Replace(validRule, "  preConfiguredDeviceMemory: \"1Gi\"\n", "", 1),
		strings.Replace(validRule, "\"1Gi\"", "null", 1),
		strings.Replace(validRule, "\"1Gi\"", "\"bad\"", 1),
		validRule + strings.TrimPrefix(validRule, "nodeConfig:\n"),
		orderedMemoryPolicy + "- name: invalid-unmatched\n  nodeSelector: {matchLabels: {pool: unrelated}}\n  preConfiguredDeviceMemory: \"bad\"\n",
	} {
		t.Run(data, func(t *testing.T) { _, err := parseDeviceMemoryPolicy([]byte(data)); require.Error(t, err) })
	}
}

func writeMemoryPolicy(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "memory.yaml")
	require.NoError(t, os.WriteFile(p, []byte(data), 0600))
	return p
}

func TestLoadDeviceMemoryBudget(t *testing.T) {
	for _, tt := range []struct {
		name     string
		enabled  bool
		data     string
		missing  bool
		want     uint64
		requests int
		fail     bool
	}{
		{name: "disabled ignores missing file", missing: true},
		{name: "disabled ignores invalid config", data: "bad: config"},
		{name: "default avoids node API", enabled: true, data: "preConfiguredDeviceMemory: \"1.1Mi\"", want: 2097152},
		{name: "empty policy", enabled: true, data: "{}"},
		{name: "selected node", enabled: true, data: orderedMemoryPolicy, want: 51539607552, requests: 1},
		{name: "missing file fails enabled", enabled: true, missing: true, fail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing")
			if !tt.missing {
				path = writeMemoryPolicy(t, tt.data)
			}
			client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{"pool": "spark", "workload": "cpu-heavy"}}})
			c := &Config{flags: &Flags{deviceMemoryConfigFile: path, nodeName: "node"}, clientsets: pkgflags.ClientSets{Core: client}}
			err := c.loadDeviceMemoryBudget(context.Background(), tt.enabled)
			if tt.fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, c.deviceMemoryBytes)
			}
			require.Len(t, client.Actions(), tt.requests)
			if tt.requests > 0 {
				require.Equal(t, "get", client.Actions()[0].GetVerb())
				require.Equal(t, "node", client.Actions()[0].(clienttesting.GetAction).GetName())
			}
		})
	}
	t.Run("no file", func(t *testing.T) {
		c := &Config{flags: &Flags{}}
		require.NoError(t, c.loadDeviceMemoryBudget(context.Background(), true))
		require.Zero(t, c.deviceMemoryBytes)
	})
}

func TestLoadDeviceMemoryNodeFailure(t *testing.T) {
	for _, failure := range []error{
		apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "node", fmt.Errorf("denied")),
		apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "node"),
		context.DeadlineExceeded,
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			client := fake.NewClientset()
			client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, failure })
			c := &Config{flags: &Flags{deviceMemoryConfigFile: writeMemoryPolicy(t, orderedMemoryPolicy), nodeName: "node"}, clientsets: pkgflags.ClientSets{Core: client}}
			err := c.loadDeviceMemoryBudget(context.Background(), true)
			require.ErrorIs(t, err, failure)
			require.Len(t, client.Actions(), 1)
			require.Zero(t, c.deviceMemoryBytes)
		})
	}
}

func TestDeviceMemoryStartupLogs(t *testing.T) {
	state := klog.CaptureState()
	defer state.Restore()
	klog.LogToStderr(false)
	var logs bytes.Buffer
	klog.SetOutput(&logs)
	c := &Config{flags: &Flags{deviceMemoryConfigFile: writeMemoryPolicy(t, `preConfiguredDeviceMemory: "1.1Mi"`)}}
	require.NoError(t, c.loadDeviceMemoryBudget(context.Background(), true))
	klog.Flush()
	for _, field := range []string{"Device memory rounded up", "requested=1.1Mi", "effective=2Mi", "effectiveBytes=2097152", "granularity=1Mi", "rounding=ceil"} {
		require.Contains(t, logs.String(), field)
	}
	require.Regexp(t, `(?m)^W[0-9]{4}`, logs.String())
	logs.Reset()
	c.flags.deviceMemoryConfigFile = writeMemoryPolicy(t, `preConfiguredDeviceMemory: "64Gi"`)
	require.NoError(t, c.loadDeviceMemoryBudget(context.Background(), true))
	klog.Flush()
	require.Contains(t, logs.String(), "Device memory policy selected")
	require.NotContains(t, logs.String(), "rounded up")
}

func TestDeviceMemoryNodeRequestCancellation(t *testing.T) {
	started := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.URL.Path
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	c := &Config{flags: &Flags{nodeName: "node", deviceMemoryConfigFile: writeMemoryPolicy(t, orderedMemoryPolicy)}, clientsets: pkgflags.ClientSets{Core: client}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.loadDeviceMemoryBudget(ctx, true) }()
	select {
	case path := <-started:
		require.Equal(t, "/api/v1/nodes/node", path)
	case <-time.After(5 * time.Second):
		t.Fatal("Node request did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Node request ignored cancellation")
	}
}
