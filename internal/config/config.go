package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultPollInterval = 60 * time.Second

const MinimumPollInterval = 5 * time.Second

type Config struct {
	AccessKeyID     string           `yaml:"access_key_id"`
	AccessKeySecret string           `yaml:"access_key_secret"`
	PollInterval    time.Duration    `yaml:"poll_interval"`
	Telegram        TelegramConfig   `yaml:"telegram"`
	Instances       []InstanceConfig `yaml:"instances"`
}

type TelegramConfig struct {
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
}

type InstanceConfig struct {
	RegionID   string   `yaml:"region_id"`
	InstanceIDs []string `yaml:"instance_ids"`
}

type rawConfig struct {
	AccessKeyID     string            `yaml:"access_key_id"`
	AccessKeySecret string            `yaml:"access_key_secret"`
	PollInterval    string            `yaml:"poll_interval"`
	Telegram        TelegramConfig    `yaml:"telegram"`
	Instances       []InstanceConfig  `yaml:"instances"`
}

type fieldError struct {
	field string
	msg   string
}

func (e fieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.field, e.msg)
}

type validationError struct {
	problems []fieldError
}

func (e *validationError) Error() string {
	parts := make([]string, 0, len(e.problems))
	for _, problem := range e.problems {
		parts = append(parts, problem.Error())
	}
	return "config validation failed: " + strings.Join(parts, "; ")
}

func (e *validationError) add(field, msg string) {
	e.problems = append(e.problems, fieldError{field: field, msg: msg})
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: invalid YAML")
	}

	cfg := &Config{
		AccessKeyID:     raw.AccessKeyID,
		AccessKeySecret: raw.AccessKeySecret,
		Telegram:        raw.Telegram,
		Instances:       raw.Instances,
	}

	if raw.PollInterval == "" {
		cfg.PollInterval = DefaultPollInterval
	} else {
		d, err := time.ParseDuration(raw.PollInterval)
		if err != nil {
			return nil, fmt.Errorf("parse config: poll_interval must be a valid duration")
		}
		cfg.PollInterval = d
	}

	if err := Validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func Validate(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config validation failed: config is required")
	}

	var errs validationError

	if strings.TrimSpace(cfg.AccessKeyID) == "" {
		errs.add("access_key_id", "is required")
	}
	if strings.TrimSpace(cfg.AccessKeySecret) == "" {
		errs.add("access_key_secret", "is required")
	}
	if cfg.PollInterval <= 0 {
		errs.add("poll_interval", "must be greater than 0")
	} else if cfg.PollInterval < MinimumPollInterval {
		errs.add("poll_interval", "must be at least 5s")
	}
	if len(cfg.Instances) == 0 {
		errs.add("instances", "must contain at least one item")
	} else {
		for i, instance := range cfg.Instances {
			prefix := fmt.Sprintf("instances[%d]", i)
			if strings.TrimSpace(instance.RegionID) == "" {
				errs.add(prefix+".region_id", "is required")
			}
			if len(instance.InstanceIDs) == 0 {
				errs.add(prefix+".instance_ids", "must contain at least one item")
			}
			for j, instanceID := range instance.InstanceIDs {
				if strings.TrimSpace(instanceID) == "" {
					errs.add(fmt.Sprintf("%s.instance_ids[%d]", prefix, j), "must not be empty")
				}
			}
		}
	}

	if len(errs.problems) > 0 {
		return &errs
	}

	return nil
}
