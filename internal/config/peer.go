package config

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/tailcat"
)

type PeerDictionaryConfig map[string]PeerConfig
type PeerConfig struct {
	Proxy      string   `yaml:"proxy"`
	ProxyURL   *url.URL `yaml:"-"`
	ApiKey     string   `yaml:"apiKey"`
	TailcatKey string   `yaml:"tailcatKey"`
	Models     []string `yaml:"models"`
	Filters    Filters  `yaml:"filters"`

	// Timeout settings for proxy connections
	Timeouts TimeoutsConfig `yaml:"timeouts"`

	// Discovery configures automatic discovery of this peer's models from
	// its own OpenAI-compatible /v1/models endpoint. A nil Discovery means
	// the block was omitted from the config entirely; a non-nil Discovery
	// with Enabled: false means the user wrote the block but turned it off.
	// Either way, Models is then required to be non-empty.
	Discovery *PeerDiscoveryConfig `yaml:"discovery"`

	tailcatBlob       string
	tailcatPrivateKey *tailcat.PrivateKey
}

// PeerDiscoveryConfig controls automatic discovery of a peer's models by
// polling an OpenAI-compatible /v1/models-shaped endpoint. It is
// intentionally limited to servers that accept plain
// "Authorization: Bearer <apiKey>" auth and return either the classic
// {"object":"list","data":[...]}  envelope or the near-identical
// {"data":[...]} envelope (e.g. OpenRouter) - see docs/configuration.md.
type PeerDiscoveryConfig struct {
	// Enabled toggles discovery. Defaults to true whenever the discovery
	// block is present at all; set explicitly to false to keep the block
	// (e.g. for documentation) without activating it.
	Enabled bool `yaml:"enabled"`

	// Path is appended to the peer's proxy URL to form the discovery
	// request, e.g. "/v1/models" (default), "/openai/v1/models" for Groq,
	// or "/v1beta/openai/models" for Gemini's OpenAI-compatible layer.
	Path string `yaml:"path"`

	// RefreshInterval is how often, in seconds, the model list is
	// re-fetched after the initial discovery. 0 means "only at startup and
	// config reload".
	RefreshInterval int `yaml:"refreshInterval"`

	// Capabilities controls whether capability/modality/context-length
	// fields from the peer's response are imported into ModelCapConfig.
	Capabilities bool `yaml:"capabilities"`

	// Include and Exclude are glob patterns (where '*' matches '/' too)
	// applied to discovered model IDs. An empty Include list means
	// "include everything". Exclude always wins over Include.
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`

	IncludeRe []*regexp.Regexp `yaml:"-"`
	ExcludeRe []*regexp.Regexp `yaml:"-"`
}

// DefaultPeerDiscoveryConfig returns the discovery settings applied when
// the discovery block is present but a given key is omitted. Exported so
// callers that build a PeerDiscoveryConfig outside of YAML unmarshalling
// (tests, and internal/router's discovery client) start from the same
// defaults as a real config load.
func DefaultPeerDiscoveryConfig() PeerDiscoveryConfig {
	return PeerDiscoveryConfig{
		Enabled:         true,
		Path:            "/v1/models",
		RefreshInterval: 300,
		Capabilities:    true,
		Include:         []string{},
		Exclude:         []string{},
	}
}

// UnmarshalYAML applies discovery defaults and compiles include/exclude
// glob patterns. Called only when the discovery block is present, so a
// nil PeerConfig.Discovery reliably distinguishes "block omitted" from
// "block present".
func (d *PeerDiscoveryConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawPeerDiscoveryConfig PeerDiscoveryConfig
	defaults := rawPeerDiscoveryConfig(DefaultPeerDiscoveryConfig())

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	if strings.TrimSpace(defaults.Path) == "" {
		return fmt.Errorf("discovery.path cannot be empty")
	}
	if defaults.RefreshInterval < 0 {
		return fmt.Errorf("discovery.refreshInterval must be >= 0")
	}

	includeRe, err := CompileGlobs(defaults.Include)
	if err != nil {
		return fmt.Errorf("discovery.include: %w", err)
	}
	excludeRe, err := CompileGlobs(defaults.Exclude)
	if err != nil {
		return fmt.Errorf("discovery.exclude: %w", err)
	}
	defaults.IncludeRe = includeRe
	defaults.ExcludeRe = excludeRe

	*d = PeerDiscoveryConfig(defaults)
	return nil
}

// Tailcat returns the Tailcat-specific peer settings derived while validating
// a tailcat:// proxy. PrivateKey is parsed key material, retained so callers do
// not need to read the key file again. found is false for non-Tailcat peers.
func (c PeerConfig) Tailcat() (key, blob string, privateKey *tailcat.PrivateKey, found bool) {
	if c.tailcatBlob == "" {
		return "", "", nil, false
	}
	return c.TailcatKey, c.tailcatBlob, c.tailcatPrivateKey, true
}

type rawPeerConfig PeerConfig

// PeerModelFQN returns the fully qualified routing name for a peer model.
// Peer and model IDs may both contain slashes, so callers must treat the
// result as an exact configured name rather than parsing it by segments.
func PeerModelFQN(peerID, modelID string) string {
	return peerID + "/" + modelID
}

// ResolvePeerModel resolves an exact fully qualified peer model name or an
// unqualified model name provided by exactly one peer. Fully qualified names
// take precedence over unqualified names that happen to contain the same text.
func (c *Config) ResolvePeerModel(search string) (peerID, modelID string, found bool) {
	for id, peer := range c.Peers {
		seen := make(map[string]struct{})
		for _, model := range peer.Models {
			if _, duplicate := seen[model]; duplicate {
				continue
			}
			seen[model] = struct{}{}
			if PeerModelFQN(id, model) != search {
				continue
			}
			if found && (peerID != id || modelID != model) {
				return "", "", false
			}
			peerID, modelID, found = id, model, true
		}
	}
	if found {
		return peerID, modelID, true
	}

	// Discovered models are addressable only by their exact fully qualified
	// name (design decision D1) - they never participate in bare-name
	// resolution, so this lookup is a single O(1) map access with no
	// ambiguity to detect.
	if dm, ok := c.PeerModels.LookupFQN(search); ok {
		return dm.PeerID, dm.ModelID, true
	}

	for id, peer := range c.Peers {
		seen := make(map[string]struct{})
		for _, model := range peer.Models {
			if _, duplicate := seen[model]; duplicate {
				continue
			}
			seen[model] = struct{}{}
			if model != search {
				continue
			}
			if found && (peerID != id || modelID != model) {
				return "", "", false
			}
			peerID, modelID, found = id, model, true
		}
	}
	return peerID, modelID, found
}

// ReservedModelNames returns every name that a peer model's fully qualified
// name is not allowed to collide with, mapped to a short human-readable
// reason. It covers local model IDs, local model aliases (including ones
// auto-registered from filters.setParamsByID), selector IDs, and pins
// across every profile - not just the currently active one, since profiles
// are switchable at runtime via PUT /api/profiles/active.
//
// This is computed once and shared by two consumers with different
// severities for the same conflict: ValidatePeerNamespace turns it into a
// load-time hard error for statically listed peer models, while peer model
// discovery uses it to silently skip (with a log warning) a dynamically
// discovered model that happens to collide. Keeping a single source of
// truth means the two can never drift apart.
func ReservedModelNames(c Config) map[string]string {
	reserved := make(map[string]string)

	for modelID := range c.Models {
		reserved[modelID] = fmt.Sprintf("model ID %q", modelID)
	}
	for modelID, model := range c.Models {
		for _, alias := range model.Aliases {
			reserved[alias] = fmt.Sprintf("model %s alias %q", modelID, alias)
		}
	}
	for alias, modelID := range c.aliases {
		if _, ok := reserved[alias]; !ok {
			reserved[alias] = fmt.Sprintf("model %s alias %q", modelID, alias)
		}
	}
	for selectorID := range c.Selectors {
		reserved[selectorID] = fmt.Sprintf("selector ID %q", selectorID)
	}
	for profileID, profile := range c.Profiles {
		for pin := range profile.Pins {
			reserved[pin] = fmt.Sprintf("profile %s pin %q", profileID, pin)
		}
	}

	return reserved
}

// ValidatePeerNamespace ensures every configured peer model has an
// unambiguous, reserved fully qualified name.
func ValidatePeerNamespace(c Config) error {
	type peerTarget struct {
		peerID  string
		modelID string
	}

	fqnTargets := make(map[string]peerTarget)
	peerIDs := make([]string, 0, len(c.Peers))
	for peerID := range c.Peers {
		peerIDs = append(peerIDs, peerID)
	}
	sort.Strings(peerIDs)

	for _, peerID := range peerIDs {
		seen := make(map[string]struct{})
		for _, modelID := range c.Peers[peerID].Models {
			if _, duplicate := seen[modelID]; duplicate {
				continue
			}
			seen[modelID] = struct{}{}

			fqn := PeerModelFQN(peerID, modelID)
			if existing, ok := fqnTargets[fqn]; ok {
				return fmt.Errorf(
					"peer model name %q is ambiguous between peers.%s model %q and peers.%s model %q",
					fqn, existing.peerID, existing.modelID, peerID, modelID,
				)
			}
			fqnTargets[fqn] = peerTarget{peerID: peerID, modelID: modelID}
		}
	}

	reserved := make([]string, 0, len(fqnTargets))
	for fqn := range fqnTargets {
		reserved = append(reserved, fqn)
	}
	sort.Strings(reserved)

	reservedNames := ReservedModelNames(c)
	for _, fqn := range reserved {
		if reason, ok := reservedNames[fqn]; ok {
			return fmt.Errorf("%s conflicts with fully qualified peer model name %q", reason, fqn)
		}
	}

	return nil
}

func (c *PeerConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	defaults := rawPeerConfig{
		Proxy:   "",
		ApiKey:  "",
		Models:  []string{},
		Filters: Filters{},

		// mostly matches http.DefaultTransport but with a 60s ResponseHeader timeout
		// to match the pre PR #619 functionality
		Timeouts: TimeoutsConfig{
			Connect:        30,
			KeepAlive:      30,
			ResponseHeader: 60,
			TLSHandshake:   10,
			ExpectContinue: 1,
			IdleConn:       90,
		},
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	// Validate proxy is not empty
	if defaults.Proxy == "" {
		return fmt.Errorf("proxy is required")
	}
	// Validate proxy is a valid URL and store the parsed value
	parsedURL, err := url.Parse(defaults.Proxy)
	if err != nil {
		return fmt.Errorf("invalid peer proxy URL (%s): %w", defaults.Proxy, err)
	}
	defaults.ProxyURL = parsedURL
	if err := validatePeerTailcat(&defaults); err != nil {
		return err
	}

	// Validate models is not empty, unless discovery is configured and
	// enabled to supply the model list at runtime instead.
	discoveryEnabled := defaults.Discovery != nil && defaults.Discovery.Enabled
	if len(defaults.Models) == 0 && !discoveryEnabled {
		return fmt.Errorf("peer models can not be empty")
	}

	*c = PeerConfig(defaults)
	return nil
}
