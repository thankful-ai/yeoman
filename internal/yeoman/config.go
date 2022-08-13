package yeoman

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	ServerConfigName = "yeoman.json"
	AppConfigName    = "app.json"
)

type LogFormat string

const (
	LogFormatDefault LogFormat = ""
	LogFormatConsole LogFormat = "console"
	LogFormatJSON    LogFormat = "json"
)

type LogLevel string

const (
	LogLevelDefault LogLevel = ""
	LogLevelDebug   LogLevel = "debug"
	LogLevelInfo    LogLevel = "info"
)

type Config struct {
	Log                LogConfig          `json:"log,omitempty"`
	ProviderRegistries []ProviderRegistry `json:"providerRegistries"`
	Store              string             `json:"store"`
}

type LogConfig struct {
	Format LogFormat `json:"format,omitempty"`
	Level  LogLevel  `json:"level,omitempty"`
}

type ProviderRegistry struct {
	Provider       string `json:"provider"`
	Registry       string `json:"registry"`
	ServiceAccount string `json:"serviceAccount"`
}

func ParseConfigForService(name string) (Config, error) {
	var conf Config
	byt, err := os.ReadFile(ServerConfigName)
	if errors.Is(err, os.ErrNotExist) {
		byt, err = os.ReadFile(filepath.Join(name, ServerConfigName))
	}
	if err != nil {
		return conf, fmt.Errorf("read file: %w", err)
	}
	if err := json.Unmarshal(byt, &conf); err != nil {
		return conf, fmt.Errorf("unmarshal: %w", err)
	}
	return conf, nil
}

func ParseConfig(configPath string) (Config, error) {
	var conf Config
	byt, err := os.ReadFile(configPath)
	if err != nil {
		return conf, fmt.Errorf("read file: %w", err)
	}
	if err := json.Unmarshal(byt, &conf); err != nil {
		return conf, fmt.Errorf("unmarshal: %w", err)
	}
	return conf, nil
}
