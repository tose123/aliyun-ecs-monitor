package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/leechao/aliyun-ecs-monitor/internal/aliyun"
	"github.com/leechao/aliyun-ecs-monitor/internal/config"
	"github.com/leechao/aliyun-ecs-monitor/internal/monitor"
	"github.com/leechao/aliyun-ecs-monitor/internal/notify"
)

type cliOptions struct {
	configPath  string
	checkConfig bool
}

type regionClient struct {
	accessKeyID     string
	accessKeySecret string

	mu      sync.Mutex
	clients map[string]aliyun.Client
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	opts, err := parseFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid flags: %v\n", err)
		return 2
	}

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config invalid: %v\n", err)
		return 1
	}

	if opts.checkConfig {
		fmt.Println("config valid")
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	notifier := notify.NewTelegramNotifier(notify.TelegramConfig{
		BotToken: cfg.Telegram.BotToken,
		ChatID:   cfg.Telegram.ChatID,
	})
	client := newRegionClient(cfg)
	mon := monitor.New(cfg, client, notifier, monitor.WithLogger(logger))

	logger.Info("monitor starting", "config", opts.configPath, "regions", countRegions(cfg), "poll_interval", cfg.PollInterval.String(), "telegram_enabled", telegramEnabled(cfg))
	if err := mon.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			logger.Info("monitor stopped")
			return 0
		}
		logger.Error("monitor stopped with error", "error", err)
		return 1
	}
	logger.Info("monitor stopped")
	return 0
}

func parseFlags(args []string) (cliOptions, error) {
	var opts cliOptions
	fs := flag.NewFlagSet("aliyun-ecs-monitor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.configPath, "config", "config.yaml", "path to config YAML")
	fs.BoolVar(&opts.checkConfig, "check-config", false, "validate config and exit")
	if err := fs.Parse(args); err != nil {
		return cliOptions{}, err
	}
	return opts, nil
}

func newRegionClient(cfg *config.Config) *regionClient {
	return &regionClient{
		accessKeyID:     cfg.AccessKeyID,
		accessKeySecret: cfg.AccessKeySecret,
		clients:         make(map[string]aliyun.Client),
	}
}

func (c *regionClient) DescribeInstanceStatuses(ctx context.Context, regionID string, instanceIDs []string) (map[string]aliyun.InstanceStatus, error) {
	client, err := c.clientForRegion(regionID)
	if err != nil {
		return nil, err
	}
	return client.DescribeInstanceStatuses(ctx, regionID, instanceIDs)
}

func (c *regionClient) StartInstance(ctx context.Context, regionID string, instanceID string) error {
	client, err := c.clientForRegion(regionID)
	if err != nil {
		return err
	}
	return client.StartInstance(ctx, regionID, instanceID)
}

func (c *regionClient) clientForRegion(regionID string) (aliyun.Client, error) {
	regionID = strings.TrimSpace(regionID)
	if regionID == "" {
		return nil, errors.New("aliyun region id is required")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if client, ok := c.clients[regionID]; ok {
		return client, nil
	}
	client, err := aliyun.NewClient(aliyun.Config{
		AccessKeyID:     c.accessKeyID,
		AccessKeySecret: c.accessKeySecret,
		RegionID:        regionID,
	})
	if err != nil {
		return nil, fmt.Errorf("create aliyun client for region %s: %w", regionID, err)
	}
	c.clients[regionID] = client
	return client, nil
}

func countRegions(cfg *config.Config) int {
	regions := make(map[string]struct{})
	for _, group := range cfg.Instances {
		regionID := strings.TrimSpace(group.RegionID)
		if regionID == "" {
			continue
		}
		regions[regionID] = struct{}{}
	}
	return len(regions)
}

func telegramEnabled(cfg *config.Config) bool {
	return strings.TrimSpace(cfg.Telegram.BotToken) != "" && strings.TrimSpace(cfg.Telegram.ChatID) != ""
}
