package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/pion/webrtc/v4"
)

type Command struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Dir         string            `json:"dir,omitempty"`
	Description string            `json:"description,omitempty"`
	InputSchema json.RawMessage   `json:"inputSchema,omitempty"`
	URL         string            `json:"url,omitempty"`
}

// DesktopConfig enables the full installed CLI and/or protected native input.
// An explicit external Command is retained for existing native-helper profiles.
type DesktopConfig struct {
	Command
	Library string `json:"library,omitempty"`
	// CLI enables the complete installed agent-desktop command surface.
	// It may coexist with Library, which retains private-pipe secret input.
	CLI string `json:"cli,omitempty"`
}

// BrowserConfig opts into an installed agent-browser CLI. Config optionally
// selects a trusted upstream configuration, not workspace auto-discovery.
type BrowserConfig struct {
	CLI    string `json:"cli"`
	Config string `json:"config,omitempty"`
}

type Config struct {
	Name    string `json:"name"`
	Gateway string `json:"gateway"`
	Token   string `json:"token,omitempty"`
	// InstructionOnly requires an exact grant even from account clients.
	// Hybrid system profiles enable it and retain a private local API.
	InstructionOnly        bool               `json:"instructionOnly,omitempty"`
	HybridSystem           bool               `json:"hybridSystem,omitempty"`
	DataDir                string             `json:"dataDir"`
	WorkDir                string             `json:"workDir"`
	Listen                 string             `json:"listen,omitempty"`
	Labels                 map[string]string  `json:"labels,omitempty"`
	RelayOnly              bool               `json:"relayOnly,omitempty"`
	ICEServers             []webrtc.ICEServer `json:"iceServers"`
	MaxTasks               int                `json:"maxTasks,omitempty"`
	MaxOperations          int                `json:"maxOperations,omitempty"`
	MaxTaskStaging         int                `json:"maxTaskStaging,omitempty"`
	MaxTaskPublications    int                `json:"maxTaskPublications,omitempty"`
	MaxDelegationsPerOwner int                `json:"maxDelegationsPerOwner,omitempty"`
	MetricsIntervalSeconds int                `json:"metricsIntervalSeconds,omitempty"`
	// TaskRetentionHours keeps terminal task records, logs, and workspaces for
	// reconciliation. Zero selects 168 hours; -1 disables age-based pruning.
	TaskRetentionHours int `json:"taskRetentionHours,omitempty"`
	// MaxRetainedTasks bounds retained terminal tasks. Zero selects 10000; -1
	// disables count-based pruning.
	MaxRetainedTasks int `json:"maxRetainedTasks,omitempty"`
	// Zero preserves unlimited artifact storage; a positive value caps blobs,
	// import staging and resumable partials together. Owned content is never evicted.
	MaxArtifactBytes      int64  `json:"maxArtifactBytes,omitempty"`
	ArtifactReserveBytes  uint64 `json:"artifactReserveBytes,omitempty"`
	PartialRetentionHours int    `json:"partialRetentionHours,omitempty"`
	MaxUploadStagingBytes int64  `json:"maxUploadStagingBytes,omitempty"`
	MaxUploadOwnerBytes   int64  `json:"maxUploadOwnerBytes,omitempty"`
	UploadReserveBytes    uint64 `json:"uploadReserveBytes,omitempty"`
	UploadRetentionHours  int    `json:"uploadRetentionHours,omitempty"`
	// Allow maps caller IDs, names, *, or @orchestrator to method glob patterns.
	// An omitted map permits same-fleet discovery and account-client execution.
	// An empty map denies incoming calls without a valid instruction/artifact grant.
	// Neither rules nor grants bypass fleet membership or task ownership.
	Allow     map[string][]string `json:"allow,omitempty"`
	Providers map[string]Command  `json:"providers,omitempty"`
	Agents    map[string]Command  `json:"agents,omitempty"`
	MCP       map[string]Command  `json:"mcp,omitempty"`
	// Desktop opts into worker-local agent-desktop control.
	Desktop *DesktopConfig `json:"desktop,omitempty"`
	Browser *BrowserConfig `json:"browser,omitempty"`
}

// UnmarshalJSON rejects the removed RPA setting instead of silently disabling
// desktop access in existing profiles. Other unknown settings remain tolerated.
func (c *Config) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key := range fields {
		if strings.EqualFold(key, "rpa") {
			return errors.New("rpa configuration is no longer supported: remove the rpa field and configure desktop.library for desktop.run/control_desktop; migrate tasks and callers explicitly, as native desktop has no action batches, screenshot artifacts, or physical input; see docs/desktop.md#migrate-from-rpa")
		}
	}
	type plain Config
	return json.Unmarshal(data, (*plain)(c))
}

func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err = json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Token == "" {
		cfg.Token = os.Getenv("CONTROL_TOKEN")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return cfg, err
	}
	if cfg.DataDir == "" {
		cfg.DataDir = ".control"
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "."
	}
	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(base, cfg.DataDir)
	}
	if !filepath.IsAbs(cfg.WorkDir) {
		cfg.WorkDir = filepath.Join(base, cfg.WorkDir)
	}
	return cfg, nil
}

func (c *Config) defaults() error {
	if c.HybridSystem && !c.InstructionOnly {
		return errors.New("hybridSystem requires instructionOnly")
	}
	if c.Name == "" || c.Gateway == "" || c.Token == "" {
		return errors.New("name, gateway, and token are required")
	}
	if c.DataDir == "" {
		c.DataDir = ".control"
	}
	if c.WorkDir == "" {
		c.WorkDir = "."
	}
	var err error
	c.DataDir, err = filepath.Abs(c.DataDir)
	if err != nil {
		return err
	}
	c.WorkDir, err = filepath.Abs(c.WorkDir)
	if err != nil {
		return err
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7331"
	}
	if c.MaxTasks <= 0 {
		c.MaxTasks = 4
	}
	if c.MaxOperations == 0 {
		c.MaxOperations = 8
	}
	if c.MaxOperations < 1 || c.MaxOperations > 256 {
		return errors.New("maxOperations must be 1..256; zero selects the eight-operation default")
	}
	if c.MaxTaskStaging == 0 {
		c.MaxTaskStaging = 4
	}
	if c.MaxTaskPublications == 0 {
		c.MaxTaskPublications = 4
	}
	if c.MaxTaskStaging < 1 || c.MaxTaskStaging > 256 || c.MaxTaskPublications < 1 || c.MaxTaskPublications > 256 {
		return errors.New("maxTaskStaging and maxTaskPublications must be 1..256; zero selects the four-task default")
	}
	if c.MaxDelegationsPerOwner == 0 {
		c.MaxDelegationsPerOwner = 1024
	}
	if c.MaxDelegationsPerOwner < 1 || c.MaxDelegationsPerOwner > maxDelegations {
		return errors.New("maxDelegationsPerOwner must be 1..4096; zero selects the 1024-grant default")
	}
	if c.MetricsIntervalSeconds == 0 {
		c.MetricsIntervalSeconds = 15
	}
	if c.MetricsIntervalSeconds < -1 || c.MetricsIntervalSeconds > 3600 {
		return errors.New("metricsIntervalSeconds must be -1 or 1..3600; zero selects the 15-second default")
	}
	if c.TaskRetentionHours == 0 {
		c.TaskRetentionHours = 168
	}
	if c.TaskRetentionHours < -1 || c.TaskRetentionHours > 87600 {
		return errors.New("taskRetentionHours must be -1 or 1..87600; zero selects the 168-hour default")
	}
	if c.MaxRetainedTasks == 0 {
		c.MaxRetainedTasks = 10000
	}
	if c.MaxRetainedTasks < -1 || c.MaxRetainedTasks > 1000000 {
		return errors.New("maxRetainedTasks must be -1 or 1..1000000; zero selects the 10000-task default")
	}
	if c.MaxArtifactBytes < 0 {
		return errors.New("maxArtifactBytes must be nonnegative")
	}
	if c.ArtifactReserveBytes == 0 {
		c.ArtifactReserveBytes = 256 << 20
	}
	if c.PartialRetentionHours == 0 {
		c.PartialRetentionHours = 168
	}
	if c.PartialRetentionHours < 1 || c.PartialRetentionHours > 87600 {
		return errors.New("partialRetentionHours must be 1..87600")
	}
	if c.MaxUploadStagingBytes < 0 || c.MaxUploadOwnerBytes < 0 || c.UploadRetentionHours < 0 || c.UploadRetentionHours > 87600 {
		return errors.New("upload byte limits must be nonnegative and uploadRetentionHours must be 0..87600")
	}
	if err = os.MkdirAll(c.DataDir, 0700); err != nil {
		return err
	}
	return os.MkdirAll(c.WorkDir, 0700)
}
