package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

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

type Config struct {
	Name                   string             `json:"name"`
	Gateway                string             `json:"gateway"`
	Token                  string             `json:"token,omitempty"`
	DataDir                string             `json:"dataDir"`
	WorkDir                string             `json:"workDir"`
	Listen                 string             `json:"listen,omitempty"`
	Labels                 map[string]string  `json:"labels,omitempty"`
	RelayOnly              bool               `json:"relayOnly,omitempty"`
	ICEServers             []webrtc.ICEServer `json:"iceServers,omitempty"`
	MaxTasks               int                `json:"maxTasks,omitempty"`
	MetricsIntervalSeconds int                `json:"metricsIntervalSeconds,omitempty"`
	// TaskRetentionHours keeps terminal task records, logs, and workspaces for
	// reconciliation. Zero selects 168 hours; -1 disables age-based pruning.
	TaskRetentionHours int `json:"taskRetentionHours,omitempty"`
	// MaxRetainedTasks bounds retained terminal tasks. Zero selects 10000; -1
	// disables count-based pruning.
	MaxRetainedTasks int `json:"maxRetainedTasks,omitempty"`
	// Allow maps enrolled caller IDs or names to capability/method glob patterns.
	// An omitted map trusts all enrolled nodes. An empty map denies all callers.
	Allow     map[string][]string `json:"allow,omitempty"`
	Providers map[string]Command  `json:"providers,omitempty"`
	Agents    map[string]Command  `json:"agents,omitempty"`
	MCP       map[string]Command  `json:"mcp,omitempty"`
	// RPA opts into desktop access through a configured version-1 helper.
	RPA *Command `json:"rpa,omitempty"`
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
	if err = os.MkdirAll(c.DataDir, 0700); err != nil {
		return err
	}
	return os.MkdirAll(c.WorkDir, 0700)
}
