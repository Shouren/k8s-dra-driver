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
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	yamlv3 "go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

const deviceMemoryStep int64 = 1 << 20

type deviceMemoryConfig struct {
	PreConfiguredDeviceMemory *string          `json:"preConfiguredDeviceMemory"`
	NodeConfig                []nodeMemoryRule `json:"nodeConfig"`
}

type nodeMemoryRule struct {
	Name                      string                `json:"name"`
	NodeSelector              *metav1.LabelSelector `json:"nodeSelector"`
	PreConfiguredDeviceMemory *string               `json:"preConfiguredDeviceMemory"`
}

type deviceMemoryBudget struct {
	source    string
	requested string
	parsed    resource.Quantity
	bytes     uint64
}

type compiledMemoryRule struct {
	selector labels.Selector
	budget   deviceMemoryBudget
}

type deviceMemoryPolicy struct {
	fallback deviceMemoryBudget
	rules    []compiledMemoryRule
}

// parseDeviceMemoryPolicy validates every rule before any node is matched.
// The slice order is the policy's precedence; maps are used only for validation.
func parseDeviceMemoryPolicy(data []byte) (*deviceMemoryPolicy, error) {
	decoder := yamlv3.NewDecoder(bytes.NewReader(data))
	var document yamlv3.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode device memory configuration: %w", err)
	}
	var extra yamlv3.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("device memory configuration must contain exactly one YAML document")
	}
	jsonData, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return nil, fmt.Errorf("decode device memory configuration: %w", err)
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yamlv3.MappingNode {
		return nil, fmt.Errorf("device memory configuration must be a mapping")
	}
	var config deviceMemoryConfig
	jsonDecoder := json.NewDecoder(bytes.NewReader(jsonData))
	jsonDecoder.DisallowUnknownFields()
	if err := jsonDecoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode device memory configuration: %w", err)
	}
	// JSON null must not silently turn an explicitly configured field into its
	// omitted/default value. Capacity values are strings and rules are a list.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(jsonData, &fields); err != nil {
		return nil, err
	}
	for _, name := range []string{"preConfiguredDeviceMemory", "nodeConfig"} {
		if bytes.Equal(fields[name], []byte("null")) {
			return nil, fmt.Errorf("device memory configuration field %q must not be null", name)
		}
	}
	rawDefault := "0"
	if config.PreConfiguredDeviceMemory != nil {
		rawDefault = *config.PreConfiguredDeviceMemory
	}
	fallback, err := normalizeDeviceMemory(rawDefault, "default")
	if err != nil {
		return nil, err
	}
	policy := &deviceMemoryPolicy{fallback: fallback}
	names := make(map[string]bool)
	for _, rule := range config.NodeConfig {
		if rule.Name == "" || names[rule.Name] {
			return nil, fmt.Errorf("device memory rule name must be nonempty and unique: %q", rule.Name)
		}
		names[rule.Name] = true
		if rule.NodeSelector == nil || len(rule.NodeSelector.MatchLabels)+len(rule.NodeSelector.MatchExpressions) == 0 {
			return nil, fmt.Errorf("device memory rule %q requires a nonempty nodeSelector", rule.Name)
		}
		selector, err := metav1.LabelSelectorAsSelector(rule.NodeSelector)
		if err != nil {
			return nil, fmt.Errorf("device memory rule %q: %w", rule.Name, err)
		}
		if rule.PreConfiguredDeviceMemory == nil {
			return nil, fmt.Errorf("device memory rule %q requires preConfiguredDeviceMemory", rule.Name)
		}
		budget, err := normalizeDeviceMemory(*rule.PreConfiguredDeviceMemory, rule.Name)
		if err != nil {
			return nil, err
		}
		policy.rules = append(policy.rules, compiledMemoryRule{selector: selector, budget: budget})
	}
	return policy, nil
}

func normalizeDeviceMemory(raw, source string) (deviceMemoryBudget, error) {
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return deviceMemoryBudget{}, fmt.Errorf("device memory %q: invalid Quantity %q: %w", source, raw, err)
	}
	// Check before Value() or multiplication, which could overflow. This also
	// rejects BinarySI inputs that ParseQuantity saturates at MaxInt64.
	maximum := resource.NewQuantity(math.MaxInt64/deviceMemoryStep*deviceMemoryStep, resource.BinarySI)
	if q.Sign() < 0 || q.Cmp(*maximum) > 0 {
		return deviceMemoryBudget{}, fmt.Errorf("device memory %q: Quantity %q must be between 0 and %s", source, raw, maximum.String())
	}
	b := q.Value()
	mib := b / deviceMemoryStep
	if b%deviceMemoryStep != 0 {
		mib++
	}
	return deviceMemoryBudget{source: source, requested: raw, parsed: q, bytes: uint64(mib * deviceMemoryStep)}, nil
}

func (p *deviceMemoryPolicy) resolve(nodeLabels map[string]string) (deviceMemoryBudget, []string) {
	budget := p.fallback
	var shadowed []string
	matched := false
	for _, rule := range p.rules {
		if !rule.selector.Matches(labels.Set(nodeLabels)) {
			continue
		}
		if !matched {
			budget = rule.budget
			matched = true
		} else {
			shadowed = append(shadowed, rule.budget.source)
		}
	}
	return budget, shadowed
}

// loadDeviceMemoryBudget is called before device discovery. Disabled HAMi-Core
// never reads the file or adds a Node request, preserving the inherited path.
func (c *Config) loadDeviceMemoryBudget(ctx context.Context, enabled bool) error {
	path := c.flags.deviceMemoryConfigFile
	if !enabled {
		if path != "" {
			klog.InfoS("Ignoring device memory configuration because HAMiCoreSupport is disabled", "path", path)
		}
		return nil
	}
	data := []byte("{}")
	if path != "" {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read device memory configuration: %w", err)
		}
	}
	policy, err := parseDeviceMemoryPolicy(data)
	if err != nil {
		return err
	}
	var nodeLabels map[string]string
	if len(policy.rules) > 0 {
		requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		node, err := c.clientsets.Core.CoreV1().Nodes().Get(requestCtx, c.flags.nodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read device memory policy labels for node %q: %w", c.flags.nodeName, err)
		}
		nodeLabels = node.Labels
	}
	budget, shadowed := policy.resolve(nodeLabels)
	c.deviceMemoryBytes = budget.bytes
	effective := resource.NewQuantity(int64(budget.bytes), resource.BinarySI)
	if budget.parsed.Cmp(*effective) != 0 {
		klog.Warningf("Device memory rounded up: source=%s requested=%s parsed=%s effective=%s effectiveBytes=%d granularity=1Mi rounding=ceil", budget.source, budget.requested, budget.parsed.String(), effective.String(), budget.bytes)
	} else {
		klog.InfoS("Device memory policy selected", "source", budget.source, "requested", budget.requested, "parsed", budget.parsed.String(), "effective", effective.String(), "effectiveBytes", budget.bytes, "granularity", "1Mi")
	}
	if len(shadowed) > 0 {
		klog.Warningf("Device memory rules shadowed by first match %q: %v", budget.source, shadowed)
	}
	return nil
}
