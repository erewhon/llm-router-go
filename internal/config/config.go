// Package config loads and validates the LLM Router model registry (models.yaml).
//
// This is a Go port of src/llm_router/config.py from the Python repo. The
// schema is intentionally identical so the same models.yaml can drive both
// stacks during the migration.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

type BackendType string

const (
	BackendVLLM     BackendType = "vllm"
	BackendLlamaCPP BackendType = "llamacpp"
	BackendLMStudio BackendType = "lmstudio"
	BackendExternal BackendType = "external"
)

type ModelCapability string

const (
	CapText        ModelCapability = "text"
	CapVision      ModelCapability = "vision"
	CapAudio       ModelCapability = "audio"
	CapImageGen    ModelCapability = "image_gen"
	CapToolCalling ModelCapability = "tool_calling"
)

type GpuType string

const (
	GpuAMD    GpuType = "amd"
	GpuNvidia GpuType = "nvidia"
	GpuIntel  GpuType = "intel"
)

type ServiceType string

const (
	ServiceComfyUI ServiceType = "comfyui"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type ServiceDefinition struct {
	Type  ServiceType `yaml:"type"`
	Port  int         `yaml:"port"`
	Label string      `yaml:"label,omitempty"`
}

type NodeDefinition struct {
	Host      string                       `yaml:"host"`
	GPU       GpuType                      `yaml:"gpu"`
	VRAMGB    int                          `yaml:"vram_gb"`
	AgentPort int                          `yaml:"agent_port,omitempty"`
	Services  map[string]ServiceDefinition `yaml:"services,omitempty"`
}

// UnmarshalYAML default-initialises AgentPort to 8100 before decoding,
// matching the Pydantic schema's default.
func (n *NodeDefinition) UnmarshalYAML(node *yaml.Node) error {
	type alias NodeDefinition
	aux := alias{AgentPort: 8100}
	if err := node.Decode(&aux); err != nil {
		return err
	}
	*n = NodeDefinition(aux)
	return nil
}

// MultiNodeConfig describes a model that spans multiple nodes via Ray TP.
type MultiNodeConfig struct {
	Nodes              []string `yaml:"nodes"`
	TensorParallelSize int      `yaml:"tensor_parallel_size"`
	HeadNode           string   `yaml:"head_node,omitempty"` // empty = first node
}

type VllmArgs struct {
	ToolCallParser       string   `yaml:"tool_call_parser,omitempty"`
	MaxModelLen          int      `yaml:"max_model_len,omitempty"`
	GPUMemoryUtilization float64  `yaml:"gpu_memory_utilization,omitempty"`
	ExtraArgs            []string `yaml:"extra_args,omitempty"`
}

// AliasOverride is per-alias overrides applied when an alias is requested.
//
// ToolProxy is *bool so a value of false (explicit opt-out) can be
// distinguished from "not set" (inherit parent model).
type AliasOverride struct {
	ChatTemplateKwargs map[string]any `yaml:"chat_template_kwargs,omitempty"`
	ToolProxy          *bool          `yaml:"tool_proxy,omitempty"`
}

type ModelDefinition struct {
	HFRepo               string                   `yaml:"hf_repo"`
	Backend              BackendType              `yaml:"backend,omitempty"`
	Node                 string                   `yaml:"node,omitempty"`
	MultiNode            *MultiNodeConfig         `yaml:"multi_node,omitempty"`
	VRAMGB               int                      `yaml:"vram_gb,omitempty"`
	AlwaysOn             bool                     `yaml:"always_on,omitempty"`
	Enabled              bool                     `yaml:"enabled"`
	ToolProxy            bool                     `yaml:"tool_proxy,omitempty"`
	Aliases              []string                 `yaml:"aliases,omitempty"`
	AliasOverrides       map[string]AliasOverride `yaml:"alias_overrides,omitempty"`
	Capabilities         []ModelCapability        `yaml:"capabilities,omitempty"`
	Tags                 []string                 `yaml:"tags,omitempty"`
	VllmArgs             VllmArgs                 `yaml:"vllm_args,omitempty"`
	GGUFFile             string                   `yaml:"gguf_file,omitempty"`
	APIPort              int                      `yaml:"api_port,omitempty"`
	APIBase              string                   `yaml:"api_base,omitempty"`
	APIKey               string                   `yaml:"api_key,omitempty"`
	InputCostPerMillion  *float64                 `yaml:"input_cost_per_million,omitempty"`
	OutputCostPerMillion *float64                 `yaml:"output_cost_per_million,omitempty"`
}

// UnmarshalYAML default-initialises Backend, Enabled, and Capabilities to
// match the Pydantic schema's defaults (vllm / true / [text]).
func (m *ModelDefinition) UnmarshalYAML(node *yaml.Node) error {
	type alias ModelDefinition
	aux := alias{
		Backend:      BackendVLLM,
		Enabled:      true,
		Capabilities: []ModelCapability{CapText},
	}
	if err := node.Decode(&aux); err != nil {
		return err
	}
	*m = ModelDefinition(aux)
	return nil
}

// ModeTag returns the value of the first "mode:xxx" tag, or "" if none is present.
func (m ModelDefinition) ModeTag() string {
	for _, t := range m.Tags {
		if strings.HasPrefix(t, "mode:") {
			return strings.SplitN(t, ":", 2)[1]
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

type ModelRegistry struct {
	Nodes  map[string]NodeDefinition  `yaml:"nodes"`
	Models map[string]ModelDefinition `yaml:"models"`
}

// Load reads and validates the registry from a YAML file.
func Load(path string) (*ModelRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return LoadBytes(data)
}

// LoadBytes parses and validates a registry from raw YAML bytes.
func LoadBytes(data []byte) (*ModelRegistry, error) {
	var r ModelRegistry
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("config: parse yaml: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate enforces the cross-field invariants that the Pydantic
// model_validator enforces in the Python schema.
func (r *ModelRegistry) Validate() error {
	var errs []error
	for id, m := range r.Models {
		if err := validateModel(id, &m, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validateModel(id string, m *ModelDefinition, r *ModelRegistry) error {
	if m.Backend == BackendExternal {
		if m.APIBase == "" {
			return fmt.Errorf("model %q: external backend requires api_base", id)
		}
		return nil
	}
	switch {
	case m.Node == "" && m.MultiNode == nil:
		return fmt.Errorf("model %q: must specify 'node' or 'multi_node'", id)
	case m.Node != "" && m.MultiNode != nil:
		return fmt.Errorf("model %q: cannot specify both 'node' and 'multi_node'", id)
	}
	if m.Node != "" {
		if _, ok := r.Nodes[m.Node]; !ok {
			return fmt.Errorf("model %q: references unknown node %q", id, m.Node)
		}
	}
	if m.MultiNode != nil {
		for _, n := range m.MultiNode.Nodes {
			if _, ok := r.Nodes[n]; !ok {
				return fmt.Errorf("model %q: multi_node references unknown node %q", id, n)
			}
		}
		if m.MultiNode.HeadNode != "" {
			if _, ok := r.Nodes[m.MultiNode.HeadNode]; !ok {
				return fmt.Errorf("model %q: multi_node.head_node %q is unknown", id, m.MultiNode.HeadNode)
			}
		}
	}
	return nil
}

// GetNode returns the node definition for a single-node model.
// Returns an error if the model is multi-node or unknown.
func (r *ModelRegistry) GetNode(modelID string) (*NodeDefinition, error) {
	m, ok := r.Models[modelID]
	if !ok {
		return nil, fmt.Errorf("config: unknown model %q", modelID)
	}
	if m.Node == "" {
		return nil, fmt.Errorf("config: model %q is multi-node", modelID)
	}
	n, ok := r.Nodes[m.Node]
	if !ok {
		return nil, fmt.Errorf("config: model %q references unknown node %q", modelID, m.Node)
	}
	return &n, nil
}

// ToolProxyAddr is the hardcoded address of the tool proxy. Matches the
// Python config.py constant. The IP avoids euclid.local mDNS instability.
const ToolProxyAddr = "http://192.168.42.240:5392/v1"

// APIBase returns the upstream API base URL for a model.
//
// For multi-node models the head node is used.
// For external models the configured api_base is returned.
// If toolProxyOverride is non-nil it replaces the model's tool_proxy flag —
// used by per-alias entries that opt in or out of tool-proxy routing.
func (r *ModelRegistry) APIBase(modelID string, toolProxyOverride *bool) (string, error) {
	m, ok := r.Models[modelID]
	if !ok {
		return "", fmt.Errorf("config: unknown model %q", modelID)
	}
	if m.APIBase != "" {
		return m.APIBase, nil
	}

	var host string
	switch {
	case m.MultiNode != nil:
		head := m.MultiNode.HeadNode
		if head == "" && len(m.MultiNode.Nodes) > 0 {
			head = m.MultiNode.Nodes[0]
		}
		n, ok := r.Nodes[head]
		if !ok {
			return "", fmt.Errorf("config: model %q head node %q unknown", modelID, head)
		}
		host = n.Host
	default:
		n, err := r.GetNode(modelID)
		if err != nil {
			return "", err
		}
		host = n.Host
	}

	effectiveToolProxy := m.ToolProxy
	if toolProxyOverride != nil {
		effectiveToolProxy = *toolProxyOverride
	}
	if effectiveToolProxy {
		return ToolProxyAddr, nil
	}

	port := m.APIPort
	if port == 0 {
		port = 5391
	}
	return fmt.Sprintf("http://%s:%d/v1", host, port), nil
}

// ModelsForNode returns the models assigned to a given node. If enabledOnly
// is true (the default behaviour in the Python helper), disabled models are
// excluded.
func (r *ModelRegistry) ModelsForNode(nodeName string, enabledOnly bool) map[string]ModelDefinition {
	out := map[string]ModelDefinition{}
	for id, m := range r.Models {
		assigned := m.Node == nodeName
		if !assigned && m.MultiNode != nil {
			for _, n := range m.MultiNode.Nodes {
				if n == nodeName {
					assigned = true
					break
				}
			}
		}
		if !assigned {
			continue
		}
		if enabledOnly && !m.Enabled {
			continue
		}
		out[id] = m
	}
	return out
}

// ModelsForMode filters models by mode tag.
//
//   - mode == "" → all enabled models, no mode filtering.
//   - mode != "" → include models tagged "mode:<mode>" plus models with no
//     mode tag; exclude models with a different mode tag.
//
// Disabled models are always excluded.
func (r *ModelRegistry) ModelsForMode(mode string) map[string]ModelDefinition {
	out := map[string]ModelDefinition{}
	for id, m := range r.Models {
		if !m.Enabled {
			continue
		}
		if mode == "" {
			out[id] = m
			continue
		}
		mt := m.ModeTag()
		if mt == "" || mt == mode {
			out[id] = m
		}
	}
	return out
}
