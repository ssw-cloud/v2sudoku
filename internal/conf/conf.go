package conf

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

const DefaultNodeRetryCount = 1
const DefaultNodeTimeout = 15

const (
	EngineEmbedded = "embedded"
	EngineExternal = "external"
)

type Conf struct {
	LogConfig     LogConfig     `mapstructure:"Log"`
	RuntimeConfig RuntimeConfig `mapstructure:"Runtime"`
	NodeConfigs   []NodeConfig  `mapstructure:"Nodes"`
}

type LogConfig struct {
	Level  string `mapstructure:"Level"`
	Output string `mapstructure:"Output"`
}

type NodeConfig struct {
	APIHost    string `mapstructure:"ApiHost"`
	NodeID     int    `mapstructure:"NodeID"`
	Key        string `mapstructure:"ApiKey"`
	Timeout    int    `mapstructure:"Timeout"`
	RetryCount *int   `mapstructure:"RetryCount"`
}

type RuntimeConfig struct {
	Engine     string `mapstructure:"Engine"`
	WorkingDir string `mapstructure:"WorkingDir"`

	// External mode starts this binary with "-c <generated config>".
	SudokuPath string `mapstructure:"SudokuPath"`

	// Fallback address is used by embedded mode when the panel does not expose one.
	FallbackAddress string `mapstructure:"FallbackAddress"`

	// ClientKeySource controls how per-user client keys are resolved.
	// Supported values: deterministic_split, uuid, deterministic, file, auto.
	ClientKeySource string `mapstructure:"ClientKeySource"`

	// ClientKeySeed is used by deterministic key derivation.
	ClientKeySeed string `mapstructure:"ClientKeySeed"`

	// ClientKeyFile stores generated keys for file/auto modes.
	ClientKeyFile string `mapstructure:"ClientKeyFile"`

	// MaxClientKeyFileUsers limits auto-generated key file growth.
	MaxClientKeyFileUsers int `mapstructure:"MaxClientKeyFileUsers"`
}

func New() *Conf {
	return &Conf{
		LogConfig: LogConfig{
			Level: "info",
		},
		RuntimeConfig: RuntimeConfig{
			Engine:                EngineEmbedded,
			WorkingDir:            "/var/lib/v2sudoku",
			SudokuPath:            "/opt/v2sudoku/sudoku",
			FallbackAddress:       "127.0.0.1:80",
			ClientKeySource:       "deterministic_split",
			ClientKeyFile:         "/var/lib/v2sudoku/client-keys.json",
			MaxClientKeyFileUsers: 10000,
		},
	}
}

func (c *Conf) LoadFromPath(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open config file error: %w", err)
	}
	defer f.Close()

	v := viper.New()
	v.SetConfigFile(filePath)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config file error: %w", err)
	}
	if err := v.Unmarshal(c); err != nil {
		return fmt.Errorf("unmarshal config error: %w", err)
	}
	for i := range c.NodeConfigs {
		if c.NodeConfigs[i].RetryCount == nil {
			c.NodeConfigs[i].RetryCount = intPtr(DefaultNodeRetryCount)
		}
	}
	c.RuntimeConfig.normalize()
	return nil
}

func intPtr(v int) *int {
	return &v
}

func (c RuntimeConfig) EngineName() string {
	engine := strings.TrimSpace(strings.ToLower(c.Engine))
	if engine == "" {
		return EngineEmbedded
	}
	return engine
}

func (c *RuntimeConfig) normalize() {
	if c.Engine == "" {
		c.Engine = EngineEmbedded
	}
	if c.WorkingDir == "" {
		c.WorkingDir = "/var/lib/v2sudoku"
	}
	if c.SudokuPath == "" {
		c.SudokuPath = "/opt/v2sudoku/sudoku"
	}
	if c.FallbackAddress == "" {
		c.FallbackAddress = "127.0.0.1:80"
	}
	if c.ClientKeySource == "" {
		c.ClientKeySource = "uuid"
	}
	if c.ClientKeyFile == "" {
		c.ClientKeyFile = "/var/lib/v2sudoku/client-keys.json"
	}
	if c.MaxClientKeyFileUsers <= 0 {
		c.MaxClientKeyFileUsers = 10000
	}
}
