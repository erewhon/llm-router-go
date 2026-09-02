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
	"sort"
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
	// GpuNone marks a CPU-only node (no discrete GPU). The agent installs
	// no GPU reader for it, so /health omits the gpu_* fields rather than
	// erroring every probe. Flip to a real vendor when a card is added
	// (e.g. hekaton's planned RTX 6000 Ada / RTX 8000).
	GpuNone GpuType = "none"
)

type ServiceType string

const (
	ServiceComfyUI ServiceType = "comfyui"
)

// APIClass declares which OpenAI endpoint family (or non-OpenAI custom
// protocol) a model speaks. The Go router uses this to decide where to
// forward a request; the agent surfaces it in /models so dashboards
// can group/filter. Default is APIClassChat — every local engine
// (vllm, sglang, llamacpp, lmstudio) speaks chat-completions.
type APIClass string

const (
	APIClassChat       APIClass = "chat"
	APIClassEmbeddings APIClass = "embeddings"
	APIClassRerank     APIClass = "rerank"
	APIClassImageGen   APIClass = "image_gen"
	APIClassImageEdit  APIClass = "image_edit"
	APIClassTTS        APIClass = "tts"
	APIClassSTT        APIClass = "stt"
	APIClassMusicGen   APIClass = "music_gen"
	// APIClassAnthropic is a transparent passthrough of the Anthropic Messages
	// API (/v1/messages). Unlike the OpenAI classes it forwards the body
	// verbatim and does not inject router credentials — the client's own
	// Authorization/x-api-key headers pass through. See internal/router/anthropic.go.
	APIClassAnthropic APIClass = "anthropic"
)

var validAPIClasses = map[APIClass]struct{}{
	APIClassChat:       {},
	APIClassEmbeddings: {},
	APIClassRerank:     {},
	APIClassImageGen:   {},
	APIClassImageEdit:  {},
	APIClassTTS:        {},
	APIClassSTT:        {},
	APIClassMusicGen:   {},
	APIClassAnthropic:  {},
}

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
	// UnifiedMemory marks a node whose RAM and VRAM are the same pool (Apple
	// Silicon, GB10 Sparks, Strix Halo). The dashboard excludes these from the
	// aggregate Fleet CPU-RAM card so their VRAM isn't double-counted as RAM.
	UnifiedMemory bool `yaml:"unified_memory,omitempty"`
	// Schedule declares when the node is *expected* to be powered on. Purely
	// informational: routing always follows observed availability, never the
	// calendar. It exists so planned downtime reads as planned rather than as
	// a fault on the dashboard.
	Schedule *NodeSchedule `yaml:"schedule,omitempty"`
}

// NodeSchedule declares a node's expected uptime windows.
type NodeSchedule struct {
	// ExpectedUp is a list of windows like "Mon-Fri 07:00-19:00". A bare
	// string is accepted as a one-element list. Empty means always expected up.
	ExpectedUp StringList `yaml:"expected_up,omitempty"`
}

// StringList decodes either a single YAML scalar or a sequence into []string,
// so `expected_up: "Mon-Fri 07:00-19:00"` and the list form both work.
type StringList []string

func (s *StringList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var one string
		if err := node.Decode(&one); err != nil {
			return err
		}
		*s = StringList{one}
		return nil
	}
	var many []string
	if err := node.Decode(&many); err != nil {
		return err
	}
	*s = StringList(many)
	return nil
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
	HFRepo         string                   `yaml:"hf_repo"`
	Backend        BackendType              `yaml:"backend,omitempty"`
	Node           string                   `yaml:"node,omitempty"`
	MultiNode      *MultiNodeConfig         `yaml:"multi_node,omitempty"`
	VRAMGB         int                      `yaml:"vram_gb,omitempty"`
	AlwaysOn       bool                     `yaml:"always_on,omitempty"`
	Enabled        bool                     `yaml:"enabled"`
	ToolProxy      bool                     `yaml:"tool_proxy,omitempty"`
	Aliases        []string                 `yaml:"aliases,omitempty"`
	AliasOverrides map[string]AliasOverride `yaml:"alias_overrides,omitempty"`
	Capabilities   []ModelCapability        `yaml:"capabilities,omitempty"`
	Tags           []string                 `yaml:"tags,omitempty"`
	VllmArgs       VllmArgs                 `yaml:"vllm_args,omitempty"`
	GGUFFile       string                   `yaml:"gguf_file,omitempty"`
	APIPort        int                      `yaml:"api_port,omitempty"`
	APIBase        string                   `yaml:"api_base,omitempty"`
	APIKey         string                   `yaml:"api_key,omitempty"`
	// APIKeyHeader, when set, carries the resolved api_key in this header
	// verbatim (raw value, no "Bearer " prefix) instead of the default
	// "Authorization: Bearer <key>". For upstreams that expect an API-key
	// header such as "X-Api-Key". Empty preserves the default bearer scheme.
	APIKeyHeader         string   `yaml:"api_key_header,omitempty"`
	APIClass             APIClass `yaml:"api_class,omitempty"`
	InputCostPerMillion  *float64 `yaml:"input_cost_per_million,omitempty"`
	OutputCostPerMillion *float64 `yaml:"output_cost_per_million,omitempty"`
	// Fallbacks is an ordered chain of other registry models tried at request
	// time when this model's upstream fails (5xx, timeout, connect error, or
	// an error envelope inside a 2xx). An entry with its own backend serves
	// itself first, then walks the chain; a "virtual" entry — fallbacks with
	// no api_base/node/multi_node of its own — is purely a routing name for
	// its chain. That is the bare-name provider-failover pattern:
	//   kimi-k3: {fallbacks: [or/kimi-k3, zen/kimi-k3]}
	// Chain members must be concrete models (no nested fallbacks) sharing the
	// entry's api_class. Unlike roles, retry also triggers on an upstream 5xx
	// or error-envelope-in-2xx, because provider chains exist precisely for
	// the endpoint-serving-500s incident shape.
	Fallbacks []string `yaml:"fallbacks,omitempty"`
	// ContextLength is the usable context window in tokens AS SERVED — the
	// engine's configured limit (llama-server --ctx-size per slot, vLLM
	// --max-model-len, Atlas --max-seq-len, a cloud provider's published
	// window), not the checkpoint's native maximum. Surfaced to clients via
	// /.well-known/opencode as limit.context. When unset the well-known
	// falls back to vllm_args.max_model_len, then a chain entry's first
	// provider, then the endpoint default.
	ContextLength int `yaml:"context_length,omitempty"`
	// MaxOutputTokens is the largest single completion the model is meant
	// to produce (the vendor's published output cap, or for local engines
	// the vendor-recommended generation length — the engine itself only
	// bounds output by remaining context). Surfaced as limit.output in
	// /.well-known/opencode; unset falls back to a chain's first provider,
	// then the endpoint default (32768).
	MaxOutputTokens int `yaml:"max_output_tokens,omitempty"`
}

// IsVirtual reports whether the entry is a pure routing name: a fallback
// chain with no backend placement of its own.
func (m ModelDefinition) IsVirtual() bool {
	return len(m.Fallbacks) > 0 && m.APIBase == "" && m.Node == "" && m.MultiNode == nil
}

// UnmarshalYAML default-initialises Backend, Enabled, Capabilities, and
// APIClass to match the Pydantic schema (Backend=vllm, Enabled=true,
// Capabilities=[text]) plus the new Go-only field default APIClass=chat.
func (m *ModelDefinition) UnmarshalYAML(node *yaml.Node) error {
	type alias ModelDefinition
	aux := alias{
		Backend:      BackendVLLM,
		Enabled:      true,
		Capabilities: []ModelCapability{CapText},
		APIClass:     APIClassChat,
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
// Roles — semantic routing handles
// ---------------------------------------------------------------------------

// Locality constrains where a role's candidates may live.
type Locality string

const (
	// LocalityAny places no constraint (the default).
	LocalityAny Locality = "any"
	// LocalityLocal admits only models pinned to a node in this fleet —
	// anything with a `node` or `multi_node`, regardless of `backend`. The
	// node-pinned externals (flux-dev, orpheus-tts, qwen3-embedding) are
	// local by this definition: they run on our hardware and die with it.
	// A nodeless external (Zen, the Anthropic gateway) is not.
	LocalityLocal Locality = "local"
)

// OnEmpty is what a role does when no candidate is available.
type OnEmpty string

const (
	// OnEmptyError returns 503 rather than violate the role's contract.
	OnEmptyError OnEmpty = "error"
	// OnEmptyOverflow falls through to the role's explicit Overflow list,
	// which is exempt from the Locality requirement.
	OnEmptyOverflow OnEmpty = "overflow"
)

// RoleRequire is the semantic contract every candidate must satisfy. It is
// enforced at load time (see validateRole), not at request time: a candidate
// that cannot satisfy the contract is a config bug, and failing startup is
// how "coder always means a local coding model" stays a guarantee rather
// than a hope.
type RoleRequire struct {
	Locality     Locality          `yaml:"locality,omitempty"`
	Capabilities []ModelCapability `yaml:"capabilities,omitempty"`
	APIClass     APIClass          `yaml:"api_class,omitempty"`
}

// RoleDefinition is one semantic routing handle: an ordered candidate list
// plus the contract binding them. Resolution walks Candidates in order and
// takes the first that is both routable in the current mode and reported
// available; see (*Router).resolveRole.
type RoleDefinition struct {
	Description string      `yaml:"description,omitempty"`
	Require     RoleRequire `yaml:"require,omitempty"`
	// Candidates are model ids in preference order. Order is the whole point:
	// it is read top-to-bottom as "best first, last resort last".
	Candidates []string `yaml:"candidates"`
	// OnEmpty selects the behaviour when no candidate is available.
	// Defaults to OnEmptyError.
	OnEmpty OnEmpty `yaml:"on_empty,omitempty"`
	// Overflow is the deliberate substitute list used only when
	// OnEmpty == OnEmptyOverflow. Exempt from Require.Locality (crossing that
	// boundary is the point of overflowing) but still bound by the rest of
	// the contract.
	Overflow []string `yaml:"overflow,omitempty"`
}

// UnmarshalYAML defaults OnEmpty to "error" and Locality to "any", so an
// unannotated role is the strict one: it never silently substitutes.
func (rd *RoleDefinition) UnmarshalYAML(node *yaml.Node) error {
	type alias RoleDefinition
	aux := alias{OnEmpty: OnEmptyError}
	if err := node.Decode(&aux); err != nil {
		return err
	}
	*rd = RoleDefinition(aux)
	if rd.Require.Locality == "" {
		rd.Require.Locality = LocalityAny
	}
	return nil
}

// IsLocal reports whether a model is pinned to fleet hardware.
func (m ModelDefinition) IsLocal() bool {
	return m.Node != "" || m.MultiNode != nil
}

// HasCapability reports whether a model declares the given capability.
func (m ModelDefinition) HasCapability(c ModelCapability) bool {
	for _, have := range m.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

type ModelRegistry struct {
	Nodes  map[string]NodeDefinition  `yaml:"nodes"`
	Models map[string]ModelDefinition `yaml:"models"`
	// Roles are semantic routing handles resolved dynamically against live
	// availability. Absent from older configs, which is fine: no roles means
	// resolution behaves exactly as it did before they existed.
	Roles map[string]RoleDefinition `yaml:"roles,omitempty"`

	// ToolProxyAddr is the address tool_proxy models are routed to. Not read
	// from YAML — set programmatically (router --tool-proxy-url flag). Empty
	// falls back to DefaultToolProxyAddr.
	ToolProxyAddr string `yaml:"-"`
}

// Load reads and validates the registry from a YAML file.
func Load(path string) (*ModelRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return LoadBytes(data)
}

// ParseBytes unmarshals a registry from raw YAML WITHOUT validating it.
//
// Almost nothing should call this: a registry that has not passed Validate may
// violate the invariants the rest of the package assumes. It exists for the
// `--validate` tooling, which needs to report every problem in a broken file —
// including advisory Lint findings — rather than stopping at the first
// Validate error. Serving code paths must use Load or LoadBytes.
func ParseBytes(data []byte) (*ModelRegistry, error) {
	var r ModelRegistry
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("config: parse yaml: %w", err)
	}
	return &r, nil
}

// LoadBytes parses and validates a registry from raw YAML bytes.
func LoadBytes(data []byte) (*ModelRegistry, error) {
	r, err := ParseBytes(data)
	if err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
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
	// Roles are validated in name order so a config with several broken roles
	// reports them deterministically (map iteration would shuffle them).
	names := make([]string, 0, len(r.Roles))
	for name := range r.Roles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		role := r.Roles[name]
		if err := validateRole(name, &role, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// validateRole enforces the role contract at load time. Everything here is a
// hard startup error: a role that can silently route outside its own semantics
// is worse than a router that refuses to boot.
func validateRole(name string, role *RoleDefinition, r *ModelRegistry) error {
	var errs []error

	// A role name must be unambiguous against every other routable name,
	// otherwise resolution order — not config — decides what "coder" means.
	if _, clash := r.Models[name]; clash {
		errs = append(errs, fmt.Errorf("role %q: collides with a model id of the same name", name))
	}
	for id, m := range r.Models {
		for _, a := range m.Aliases {
			if a == name {
				errs = append(errs, fmt.Errorf("role %q: collides with an alias on model %q; remove the alias", name, id))
			}
		}
	}

	switch role.OnEmpty {
	case OnEmptyError:
		if len(role.Overflow) > 0 {
			errs = append(errs, fmt.Errorf("role %q: overflow list set but on_empty is %q; set on_empty: overflow or drop the list", name, OnEmptyError))
		}
	case OnEmptyOverflow:
		if len(role.Overflow) == 0 {
			errs = append(errs, fmt.Errorf("role %q: on_empty is %q but overflow list is empty", name, OnEmptyOverflow))
		}
	default:
		errs = append(errs, fmt.Errorf("role %q: unknown on_empty %q (want %q or %q)", name, role.OnEmpty, OnEmptyError, OnEmptyOverflow))
	}

	switch role.Require.Locality {
	case LocalityAny, LocalityLocal:
	default:
		errs = append(errs, fmt.Errorf("role %q: unknown require.locality %q (want %q or %q)", name, role.Require.Locality, LocalityAny, LocalityLocal))
	}
	if role.Require.APIClass != "" {
		if _, ok := validAPIClasses[role.Require.APIClass]; !ok {
			errs = append(errs, fmt.Errorf("role %q: unknown require.api_class %q", name, role.Require.APIClass))
		}
	}

	if len(role.Candidates) == 0 {
		errs = append(errs, fmt.Errorf("role %q: needs at least one candidate", name))
	}

	// Candidates bear the full contract; overflow entries are exempt from
	// locality only — crossing that boundary is precisely what they are for.
	for _, id := range role.Candidates {
		errs = append(errs, roleMemberErrs(name, "candidate", id, role, r, true)...)
	}
	for _, id := range role.Overflow {
		errs = append(errs, roleMemberErrs(name, "overflow", id, role, r, false)...)
	}

	return errors.Join(errs...)
}

// roleMemberErrs checks one candidate/overflow entry against the role
// contract. enforceLocality is false for overflow entries.
func roleMemberErrs(role, kind, id string, rd *RoleDefinition, r *ModelRegistry, enforceLocality bool) []error {
	m, ok := r.Models[id]
	if !ok {
		return []error{fmt.Errorf("role %q: %s %q is not a known model", role, kind, id)}
	}
	var errs []error
	if enforceLocality && rd.Require.Locality == LocalityLocal && !m.IsLocal() {
		errs = append(errs, fmt.Errorf("role %q: %s %q is not local (no node/multi_node) but require.locality is %q", role, kind, id, LocalityLocal))
	}
	for _, want := range rd.Require.Capabilities {
		if !m.HasCapability(want) {
			errs = append(errs, fmt.Errorf("role %q: %s %q lacks required capability %q", role, kind, id, want))
		}
	}
	if rd.Require.APIClass != "" && m.APIClass != rd.Require.APIClass {
		errs = append(errs, fmt.Errorf("role %q: %s %q has api_class %q, role requires %q", role, kind, id, m.APIClass, rd.Require.APIClass))
	}
	return errs
}

func validateModel(id string, m *ModelDefinition, r *ModelRegistry) error {
	if _, ok := validAPIClasses[m.APIClass]; !ok {
		return fmt.Errorf("model %q: unknown api_class %q", id, m.APIClass)
	}
	if err := validateFallbacks(id, m, r); err != nil {
		return err
	}
	// A virtual entry is exempt from placement rules: its chain members carry
	// the real backends.
	if m.IsVirtual() {
		return nil
	}
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

// validateFallbacks enforces the chain contract: members exist, are concrete
// (no nested chains — which also rules out cycles), aren't the entry itself,
// and share the entry's api_class so a chain can never answer with the wrong
// endpoint family.
func validateFallbacks(id string, m *ModelDefinition, r *ModelRegistry) error {
	var errs []error
	for _, fid := range m.Fallbacks {
		if fid == id {
			errs = append(errs, fmt.Errorf("model %q: fallback names itself", id))
			continue
		}
		fm, ok := r.Models[fid]
		if !ok {
			errs = append(errs, fmt.Errorf("model %q: fallback %q is not a known model", id, fid))
			continue
		}
		if len(fm.Fallbacks) > 0 {
			errs = append(errs, fmt.Errorf("model %q: fallback %q has fallbacks of its own; chains must be flat", id, fid))
		}
		if fm.APIClass != m.APIClass {
			errs = append(errs, fmt.Errorf("model %q: fallback %q has api_class %q, chain requires %q", id, fid, fm.APIClass, m.APIClass))
		}
	}
	return errors.Join(errs...)
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

// DefaultToolProxyAddr is the fallback address of the tool proxy, used when
// ModelRegistry.ToolProxyAddr is unset. Matches the Python config.py constant.
// The IP (not euclid.local) avoids mDNS instability. Override per-instance via
// the router's --tool-proxy-url flag / $ROUTER_TOOL_PROXY_URL so a second
// router host (HA) can point at a different tool proxy without a recompile.
const DefaultToolProxyAddr = "http://192.168.42.240:5392/v1"

// DefaultAPIPort is the port assumed for a single-node model that declares no
// api_port. Named so the lint checks agree with APIBase on what "no port set"
// actually resolves to — a collision check that guessed a different default
// would report addresses the router never uses.
const DefaultAPIPort = 5391

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
		if r.ToolProxyAddr != "" {
			return r.ToolProxyAddr, nil
		}
		return DefaultToolProxyAddr, nil
	}

	port := m.APIPort
	if port == 0 {
		port = DefaultAPIPort
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

// RolesForMode returns the roles with their Candidates and Overflow lists
// pre-filtered to the models routable in the given mode (same filter as
// ModelsForMode). This is how mode tags keep working under roles: a candidate
// carrying the wrong mode tag — or disabled entirely, as the rollback entries
// are — simply drops out of the preference order instead of being selected and
// then failing.
//
// Roles left with no candidates AND no overflow are dropped: they cannot
// resolve to anything in this mode, so advertising them in /v1/models or the
// well-known would be a lie.
func (r *ModelRegistry) RolesForMode(mode string) map[string]RoleDefinition {
	active := r.ModelsForMode(mode)
	keep := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if _, ok := active[id]; ok {
				out = append(out, id)
			}
		}
		return out
	}

	out := make(map[string]RoleDefinition, len(r.Roles))
	for name, role := range r.Roles {
		role.Candidates = keep(role.Candidates)
		role.Overflow = keep(role.Overflow)
		if len(role.Candidates) == 0 && len(role.Overflow) == 0 {
			continue
		}
		out[name] = role
	}
	return out
}
