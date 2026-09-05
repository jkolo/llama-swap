package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPeerConfig_UnmarshalYAML(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "valid config",
			yaml: `
proxy: http://192.168.1.23
models:
  - model_a
  - model_b
`,
			wantErr: "",
		},
		{
			name: "valid config with apiKey",
			yaml: `
proxy: https://openrouter.ai/api
apiKey: sk-test-key
models:
  - meta-llama/llama-3.1-8b-instruct
`,
			wantErr: "",
		},
		{
			name: "missing proxy",
			yaml: `
models:
  - model_a
`,
			wantErr: "proxy is required",
		},
		{
			name: "empty proxy",
			yaml: `
proxy: ""
models:
  - model_a
`,
			wantErr: "proxy is required",
		},
		{
			name: "invalid proxy URL",
			yaml: `
proxy: "://invalid"
models:
  - model_a
`,
			wantErr: "invalid peer proxy URL",
		},
		{
			name: "missing models",
			yaml: `
proxy: http://localhost:8080
`,
			wantErr: "peer models can not be empty",
		},
		{
			name: "empty models",
			yaml: `
proxy: http://localhost:8080
models: []
`,
			wantErr: "peer models can not be empty",
		},
		{
			name: "missing models but discovery enabled",
			yaml: `
proxy: http://localhost:8080
discovery:
  enabled: true
`,
			wantErr: "",
		},
		{
			name: "missing models with discovery block present, no enabled key (defaults to enabled)",
			yaml: `
proxy: http://localhost:8080
discovery: {}
`,
			wantErr: "",
		},
		{
			name: "missing models with discovery explicitly disabled still errors",
			yaml: `
proxy: http://localhost:8080
discovery:
  enabled: false
`,
			wantErr: "peer models can not be empty",
		},
		{
			name: "discovery with negative refreshInterval",
			yaml: `
proxy: http://localhost:8080
discovery:
  refreshInterval: -1
models:
  - model_a
`,
			wantErr: "discovery.refreshInterval",
		},
		{
			name: "discovery with refreshInterval above the maximum",
			yaml: `
proxy: http://localhost:8080
discovery:
  refreshInterval: 10000000000
models:
  - model_a
`,
			wantErr: "discovery.refreshInterval",
		},
		{
			name: "discovery with refreshInterval at the maximum",
			yaml: `
proxy: http://localhost:8080
discovery:
  refreshInterval: 315360000
models:
  - model_a
`,
			wantErr: "",
		},
		{
			name: "discovery with negative retryInterval",
			yaml: `
proxy: http://localhost:8080
discovery:
  retryInterval: -1
models:
  - model_a
`,
			wantErr: "discovery.retryInterval",
		},
		{
			name: "discovery with retryInterval above the maximum",
			yaml: `
proxy: http://localhost:8080
discovery:
  retryInterval: 10000000000
models:
  - model_a
`,
			wantErr: "discovery.retryInterval",
		},
		{
			name: "discovery with retryInterval of zero disables retry",
			yaml: `
proxy: http://localhost:8080
discovery:
  retryInterval: 0
models:
  - model_a
`,
			wantErr: "",
		},
		{
			name: "discovery with empty path",
			yaml: `
proxy: http://localhost:8080
discovery:
  path: ""
models:
  - model_a
`,
			wantErr: "discovery.path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var config PeerConfig
			err := yaml.Unmarshal([]byte(tt.yaml), &config)

			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErr)
				} else if !contains(err.Error(), tt.wantErr) {
					t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
			}
		})
	}
}

func TestPeerConfig_DiscoveryDefaults(t *testing.T) {
	var cfg PeerConfig
	err := yaml.Unmarshal([]byte(`
proxy: https://openrouter.ai/api
discovery: {}
`), &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Discovery == nil {
		t.Fatal("expected Discovery to be non-nil when the block is present")
	}
	if !cfg.Discovery.Enabled {
		t.Error("expected Enabled to default to true when the block is present")
	}
	if cfg.Discovery.Path != "/v1/models" {
		t.Errorf("expected default path /v1/models, got %q", cfg.Discovery.Path)
	}
	if cfg.Discovery.RefreshInterval != 300 {
		t.Errorf("expected default refreshInterval 300, got %d", cfg.Discovery.RefreshInterval)
	}
	if cfg.Discovery.RetryInterval != 15 {
		t.Errorf("expected default retryInterval 15, got %d", cfg.Discovery.RetryInterval)
	}
	if !cfg.Discovery.Capabilities {
		t.Error("expected Capabilities to default to true")
	}
	if len(cfg.Discovery.Include) != 0 || len(cfg.Discovery.Exclude) != 0 {
		t.Error("expected empty include/exclude by default")
	}
}

func TestPeerConfig_DiscoveryAbsentWhenBlockOmitted(t *testing.T) {
	var cfg PeerConfig
	err := yaml.Unmarshal([]byte(`
proxy: https://openrouter.ai/api
models:
  - model_a
`), &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Discovery != nil {
		t.Fatal("expected Discovery to be nil when the block is omitted")
	}
}

func TestPeerConfig_DiscoveryCompilesGlobs(t *testing.T) {
	var cfg PeerConfig
	err := yaml.Unmarshal([]byte(`
proxy: https://openrouter.ai/api
discovery:
  include:
    - "openai/*"
  exclude:
    - "*:free"
`), &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Discovery.IncludeRe) != 1 || !cfg.Discovery.IncludeRe[0].MatchString("openai/gpt-4o") {
		t.Error("expected include glob to compile and match")
	}
	if len(cfg.Discovery.ExcludeRe) != 1 || !cfg.Discovery.ExcludeRe[0].MatchString("z-ai/glm-4.7:free") {
		t.Error("expected exclude glob to compile and match")
	}
}

func TestPeerConfig_DiscoveryExplicitOverrides(t *testing.T) {
	var cfg PeerConfig
	err := yaml.Unmarshal([]byte(`
proxy: https://openrouter.ai/api
discovery:
  enabled: false
  path: /openai/v1/models
  refreshInterval: 60
  retryInterval: 5
  capabilities: false
models:
  - model_a
`), &cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Discovery.Enabled {
		t.Error("expected Enabled to be false when explicitly set")
	}
	if cfg.Discovery.Path != "/openai/v1/models" {
		t.Errorf("expected explicit path, got %q", cfg.Discovery.Path)
	}
	if cfg.Discovery.RefreshInterval != 60 {
		t.Errorf("expected explicit refreshInterval 60, got %d", cfg.Discovery.RefreshInterval)
	}
	if cfg.Discovery.RetryInterval != 5 {
		t.Errorf("expected explicit retryInterval 5, got %d", cfg.Discovery.RetryInterval)
	}
	if cfg.Discovery.Capabilities {
		t.Error("expected Capabilities to be false when explicitly set")
	}
}

func TestPeerConfig_ProxyURL(t *testing.T) {
	yamlData := `
proxy: http://192.168.1.23:8080/api
apiKey: sk-test
models:
  - model_a
`
	var config PeerConfig
	err := yaml.Unmarshal([]byte(yamlData), &config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if config.ProxyURL == nil {
		t.Fatal("ProxyURL should not be nil")
	}

	if config.ProxyURL.Host != "192.168.1.23:8080" {
		t.Errorf("expected host %q, got %q", "192.168.1.23:8080", config.ProxyURL.Host)
	}

	if config.ProxyURL.Scheme != "http" {
		t.Errorf("expected scheme %q, got %q", "http", config.ProxyURL.Scheme)
	}

	if config.ProxyURL.Path != "/api" {
		t.Errorf("expected path %q, got %q", "/api", config.ProxyURL.Path)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestPeerConfig_WithFilters(t *testing.T) {
	yamlData := `
proxy: https://openrouter.ai/api
apiKey: sk-test
models:
  - model_a
filters:
  setParams:
    temperature: 0.7
    provider:
      data_collection: deny
`
	var config PeerConfig
	err := yaml.Unmarshal([]byte(yamlData), &config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if config.Filters.SetParams == nil {
		t.Fatal("Filters.SetParams should not be nil")
	}

	if config.Filters.SetParams["temperature"] != 0.7 {
		t.Errorf("expected temperature 0.7, got %v", config.Filters.SetParams["temperature"])
	}

	provider, ok := config.Filters.SetParams["provider"].(map[string]any)
	if !ok {
		t.Fatal("provider should be a map")
	}
	if provider["data_collection"] != "deny" {
		t.Errorf("expected data_collection deny, got %v", provider["data_collection"])
	}
}

func TestPeerConfig_WithBothFilters(t *testing.T) {
	yamlData := `
proxy: https://openrouter.ai/api
apiKey: sk-test
models:
  - model_a
filters:
  stripParams: "temperature, top_p"
  setParams:
    max_tokens: 1000
`
	var config PeerConfig
	err := yaml.Unmarshal([]byte(yamlData), &config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check stripParams
	stripParams := config.Filters.SanitizedStripParams()
	if len(stripParams) != 2 {
		t.Errorf("expected 2 strip params, got %d", len(stripParams))
	}
	if stripParams[0] != "temperature" || stripParams[1] != "top_p" {
		t.Errorf("unexpected strip params: %v", stripParams)
	}

	// Check setParams
	if config.Filters.SetParams == nil {
		t.Fatal("Filters.SetParams should not be nil")
	}
	if config.Filters.SetParams["max_tokens"] != 1000 {
		t.Errorf("expected max_tokens 1000, got %v", config.Filters.SetParams["max_tokens"])
	}
}

func TestConfig_ResolvePeerModel(t *testing.T) {
	cfg := Config{
		Models: map[string]ModelConfig{"shared": {}},
		Peers: PeerDictionaryConfig{
			"p1": {Models: []string{"shared", "org/model"}},
			"p2": {Models: []string{"shared", "unique"}},
		},
	}

	tests := []struct {
		search      string
		wantPeerID  string
		wantModelID string
		wantFound   bool
	}{
		{search: "p1/shared", wantPeerID: "p1", wantModelID: "shared", wantFound: true},
		{search: "p2/shared", wantPeerID: "p2", wantModelID: "shared", wantFound: true},
		{search: "p1/org/model", wantPeerID: "p1", wantModelID: "org/model", wantFound: true},
		{search: "unique", wantPeerID: "p2", wantModelID: "unique", wantFound: true},
		{search: "shared", wantFound: false},
		{search: "missing", wantFound: false},
	}

	for _, tt := range tests {
		t.Run(tt.search, func(t *testing.T) {
			peerID, modelID, found := cfg.ResolvePeerModel(tt.search)
			if peerID != tt.wantPeerID || modelID != tt.wantModelID || found != tt.wantFound {
				t.Fatalf(
					"ResolvePeerModel(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.search, peerID, modelID, found,
					tt.wantPeerID, tt.wantModelID, tt.wantFound,
				)
			}
		})
	}

	if got, found := cfg.ResolveBaseModel("shared"); !found || got != "shared" {
		t.Fatalf("ResolveBaseModel(shared) = (%q, %v), want local model", got, found)
	}
	if got, found := cfg.ResolveBaseModel("p1/shared"); !found || got != "p1/shared" {
		t.Fatalf("ResolveBaseModel(p1/shared) = (%q, %v), want peer FQN", got, found)
	}
}

func TestConfig_ResolvePeerModel_DiscoveredFQN(t *testing.T) {
	registry := NewPeerRegistry()
	registry.SetPeerModels("openrouter", map[string]DiscoveredModel{
		"z-ai/glm-4.7": {PeerID: "openrouter", ModelID: "z-ai/glm-4.7"},
	})
	cfg := Config{PeerModels: registry}

	peerID, modelID, found := cfg.ResolvePeerModel("openrouter/z-ai/glm-4.7")
	if !found || peerID != "openrouter" || modelID != "z-ai/glm-4.7" {
		t.Fatalf("ResolvePeerModel(discovered FQN) = (%q, %q, %v), want match", peerID, modelID, found)
	}
}

func TestConfig_ResolvePeerModel_DiscoveredModelsAreFQNOnly(t *testing.T) {
	// Per design decision D1: discovered models never get a "bare" alias,
	// even when only one peer has discovered a model under that ID. Bare
	// aliasing remains exclusive to statically configured peer.models,
	// because a bare alias to a discovered model would depend on remote
	// state that can change asynchronously between two peers' independent
	// refresh cycles.
	registry := NewPeerRegistry()
	registry.SetPeerModels("openrouter", map[string]DiscoveredModel{
		"unique-model": {PeerID: "openrouter", ModelID: "unique-model"},
	})
	cfg := Config{PeerModels: registry}

	_, _, found := cfg.ResolvePeerModel("unique-model")
	if found {
		t.Fatal("expected a bare name to NOT resolve to a discovered-only model")
	}

	// The FQN form still works.
	peerID, modelID, found := cfg.ResolvePeerModel("openrouter/unique-model")
	if !found || peerID != "openrouter" || modelID != "unique-model" {
		t.Fatalf("ResolvePeerModel(FQN) = (%q, %q, %v), want match", peerID, modelID, found)
	}
}

func TestConfig_ResolvePeerModel_StaticWinsOverDiscoveredOnFQNCollision(t *testing.T) {
	registry := NewPeerRegistry()
	registry.SetPeerModels("openrouter", map[string]DiscoveredModel{
		"model-a": {PeerID: "openrouter", ModelID: "model-a", Name: "stale discovered copy"},
	})
	cfg := Config{
		Peers:      PeerDictionaryConfig{"openrouter": {Models: []string{"model-a"}}},
		PeerModels: registry,
	}

	peerID, modelID, found := cfg.ResolvePeerModel("openrouter/model-a")
	if !found || peerID != "openrouter" || modelID != "model-a" {
		t.Fatalf("ResolvePeerModel = (%q, %q, %v), want the statically configured entry", peerID, modelID, found)
	}
}

func TestConfig_ResolvePeerModel_NilRegistrySafe(t *testing.T) {
	cfg := Config{Peers: PeerDictionaryConfig{"p1": {Models: []string{"model"}}}}
	// PeerModels is nil (the zero value config.Config{} literal used
	// throughout the test suite) - must not panic.
	_, _, found := cfg.ResolvePeerModel("p1/nonexistent")
	if found {
		t.Fatal("expected no match")
	}
}

func TestConfig_ResolvePeerModel_FQNPrecedesBareName(t *testing.T) {
	cfg := Config{Peers: PeerDictionaryConfig{
		"p1": {Models: []string{"model"}},
		"p2": {Models: []string{"p1/model"}},
	}}

	peerID, modelID, found := cfg.ResolvePeerModel("p1/model")
	if !found || peerID != "p1" || modelID != "model" {
		t.Fatalf("ResolvePeerModel(p1/model) = (%q, %q, %v), want p1/model FQN", peerID, modelID, found)
	}

	peerID, modelID, found = cfg.ResolvePeerModel("p2/p1/model")
	if !found || peerID != "p2" || modelID != "p1/model" {
		t.Fatalf("ResolvePeerModel(p2/p1/model) = (%q, %q, %v), want p2 raw slash model", peerID, modelID, found)
	}
}

func TestConfig_ReservedModelNames(t *testing.T) {
	cfg := Config{
		Models: map[string]ModelConfig{
			"local-a": {Aliases: []string{"alias-a"}},
			"local-b": {},
		},
		Selectors: map[string]SelectorConfig{
			"any-chat": {},
		},
		Profiles: map[string]ProfileConfig{
			"work":   {Pins: map[string]string{"work-pin": "local-a"}},
			"gaming": {Pins: map[string]string{"gaming-pin": "local-b"}},
		},
	}
	cfg.aliases = map[string]string{"alias-a": "local-a"}

	reserved := ReservedModelNames(cfg)

	for _, name := range []string{"local-a", "local-b", "alias-a", "any-chat", "work-pin", "gaming-pin"} {
		if _, ok := reserved[name]; !ok {
			t.Errorf("expected %q to be reserved", name)
		}
	}

	// Reasons should be descriptive enough to explain the conflict in an
	// error message.
	if !strings.Contains(reserved["local-a"], "model ID") {
		t.Errorf("expected reason for local-a to mention model ID, got %q", reserved["local-a"])
	}
	if !strings.Contains(reserved["alias-a"], "alias") {
		t.Errorf("expected reason for alias-a to mention alias, got %q", reserved["alias-a"])
	}
	if !strings.Contains(reserved["any-chat"], "selector") {
		t.Errorf("expected reason for any-chat to mention selector, got %q", reserved["any-chat"])
	}
	if !strings.Contains(reserved["work-pin"], "profile") {
		t.Errorf("expected reason for work-pin to mention profile, got %q", reserved["work-pin"])
	}

	if _, ok := reserved["not-reserved"]; ok {
		t.Error("expected unrelated name to not be reserved")
	}
}

func TestConfig_ValidatePeerNamespace(t *testing.T) {
	t.Run("ambiguous constructed name", func(t *testing.T) {
		cfg := Config{Peers: PeerDictionaryConfig{
			"a":   {Models: []string{"b/c"}},
			"a/b": {Models: []string{"c"}},
		}}
		if err := ValidatePeerNamespace(cfg); err == nil {
			t.Fatal("expected ambiguous peer FQN error")
		}
	})

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "local model",
			mutate: func(cfg *Config) {
				cfg.Models["peer/model"] = ModelConfig{}
			},
		},
		{
			name: "local alias",
			mutate: func(cfg *Config) {
				cfg.Models["local"] = ModelConfig{Aliases: []string{"peer/model"}}
			},
		},
		{
			name: "selector",
			mutate: func(cfg *Config) {
				cfg.Selectors["peer/model"] = SelectorConfig{}
			},
		},
		{
			name: "profile pin",
			mutate: func(cfg *Config) {
				cfg.Profiles["profile"] = ProfileConfig{Pins: map[string]string{"peer/model": "local"}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Models:    map[string]ModelConfig{},
				Peers:     PeerDictionaryConfig{"peer": {Models: []string{"model"}}},
				Selectors: map[string]SelectorConfig{},
				Profiles:  map[string]ProfileConfig{},
			}
			tt.mutate(&cfg)
			if err := ValidatePeerNamespace(cfg); err == nil {
				t.Fatal("expected reserved peer FQN conflict")
			}
		})
	}
}

func TestConfig_LoadConfigFromReader_AllocatesPeerModelsRegistry(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
models:
  local:
    cmd: echo ${PORT}
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PeerModels == nil {
		t.Fatal("expected LoadConfigFromReader to always allocate a non-nil PeerModels registry")
	}
	if got := cfg.PeerModels.Models(); len(got) != 0 {
		t.Fatalf("expected an empty registry on load, got %d entries", len(got))
	}
}

func TestConfig_LoadPeerNamespaceConflict(t *testing.T) {
	_, err := LoadConfigFromReader(strings.NewReader(`
models:
  local:
    cmd: echo ${PORT}
    filters:
      setParamsByID:
        peer/model:
          temperature: 0.5
peers:
  peer:
    proxy: http://example.com
    models: [model]
`))
	if err == nil || !strings.Contains(err.Error(), "conflicts with fully qualified peer model name") {
		t.Fatalf("LoadConfigFromReader error = %v, want peer FQN conflict", err)
	}
}
