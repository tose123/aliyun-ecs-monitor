package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/leechao/aliyun-ecs-monitor/internal/aliyun"
	"github.com/leechao/aliyun-ecs-monitor/internal/config"
	"github.com/leechao/aliyun-ecs-monitor/internal/notify"
)

const (
	ConfirmationPollInterval = 10 * time.Second
	StartConfirmationTimeout = 5 * time.Minute
	MaxTransientAttempts     = 3
)

const (
	statusRunning  = "Running"
	statusStarting = "Starting"
	statusStopping = "Stopping"
	statusStopped  = "Stopped"
)

type Monitor struct {
	cfg      *config.Config
	client   aliyun.Client
	notifier notify.Notifier
	logger   *slog.Logger

	inFlightMu sync.Mutex
	inFlight   map[string]struct{}
	wg         sync.WaitGroup
}

type Option func(*Monitor)

func WithLogger(logger *slog.Logger) Option {
	return func(m *Monitor) {
		if logger != nil {
			m.logger = logger
		}
	}
}

func New(cfg *config.Config, client aliyun.Client, notifier notify.Notifier, opts ...Option) *Monitor {
	if notifier == nil {
		notifier = notify.NewNoop()
	}
	monitor := &Monitor{
		cfg:      cfg,
		client:   client,
		notifier: notifier,
		logger:   slog.Default(),
		inFlight: make(map[string]struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(monitor)
		}
	}
	return monitor
}

func (m *Monitor) Run(ctx context.Context) error {
	if err := m.pollOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		m.logger.Warn("monitor poll failed", "error", err)
	}

	ticker := time.NewTicker(m.pollInterval())
	defer ticker.Stop()
	defer m.wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := m.pollOnce(ctx); err != nil {
				if errors.Is(err, context.Canceled) {
					return ctx.Err()
				}
				m.logger.Warn("monitor poll failed", "error", err)
			}
		}
	}
}

func (m *Monitor) pollOnce(ctx context.Context) error {
	if m == nil || m.cfg == nil || m.client == nil {
		return errors.New("monitor requires config and aliyun client")
	}
	for _, group := range m.cfg.Instances {
		if err := ctx.Err(); err != nil {
			return err
		}
		regionID := strings.TrimSpace(group.RegionID)
		instanceIDs := normalizeInstanceIDs(group.InstanceIDs)
		if regionID == "" || len(instanceIDs) == 0 {
			continue
		}

		statuses, err := retryTransient(ctx, MaxTransientAttempts, func(ctx context.Context) (map[string]aliyun.InstanceStatus, error) {
			return m.client.DescribeInstanceStatuses(ctx, regionID, instanceIDs)
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			m.logger.Warn("describe instance statuses failed", "region", regionID, "error", err)
			m.notify(ctx, notify.Message{RegionID: regionID, EventType: notify.EventMonitorError, ErrorSummary: safeSummary(err), Detail: "describe instance statuses failed"})
			continue
		}

		for _, instanceID := range instanceIDs {
			status, ok := statuses[instanceID]
			if !ok {
				m.handleMissingStatus(ctx, regionID, instanceID)
				continue
			}
			m.handleStatus(ctx, regionID, instanceID, strings.TrimSpace(status.Status))
		}
	}
	return nil
}

func (m *Monitor) handleStatus(ctx context.Context, regionID, instanceID, status string) {
	switch status {
	case statusRunning:
		m.logger.Info("instance running", "region", regionID, "instance", instanceID)
	case statusStarting, statusStopping:
		m.logger.Info("instance transitional", "region", regionID, "instance", instanceID, "status", status)
	case statusStopped:
		m.handleStopped(ctx, regionID, instanceID)
	default:
		m.logger.Warn("instance status unknown", "region", regionID, "instance", instanceID, "status", status)
		m.notify(ctx, notify.Message{RegionID: regionID, InstanceID: instanceID, EventType: notify.EventMonitorError, ErrorSummary: "unknown instance status", Detail: "status=" + emptyFallback(status, "missing")})
	}
}

func (m *Monitor) handleStopped(ctx context.Context, regionID, instanceID string) {
	key := instanceKey(regionID, instanceID)
	if !m.markInFlight(key) {
		m.logger.Info("start already in flight", "region", regionID, "instance", instanceID)
		return
	}

	m.notify(ctx, notify.Message{RegionID: regionID, InstanceID: instanceID, EventType: notify.EventStoppedDetected, ErrorSummary: "instance stopped"})
	_, err := retryTransient(ctx, MaxTransientAttempts, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, m.client.StartInstance(ctx, regionID, instanceID)
	})
	if err != nil {
		m.clearInFlight(key)
		if errors.Is(err, context.Canceled) {
			return
		}
		m.logger.Warn("start instance failed", "region", regionID, "instance", instanceID, "error", err)
		m.notify(ctx, notify.Message{RegionID: regionID, InstanceID: instanceID, EventType: notify.EventStartFailed, ErrorSummary: safeSummary(err)})
		return
	}

	m.notify(ctx, notify.Message{RegionID: regionID, InstanceID: instanceID, EventType: notify.EventStartAccepted, ErrorSummary: "start request accepted"})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.clearInFlight(key)
		m.confirmRunning(ctx, regionID, instanceID)
	}()
}

func (m *Monitor) confirmRunning(parent context.Context, regionID, instanceID string) {
	ctx, cancel := context.WithTimeout(parent, StartConfirmationTimeout)
	defer cancel()

	ticker := time.NewTicker(ConfirmationPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return
			}
			m.logger.Warn("start confirmation timed out", "region", regionID, "instance", instanceID)
			m.notify(parent, notify.Message{RegionID: regionID, InstanceID: instanceID, EventType: notify.EventStartTimeout, ErrorSummary: "instance did not reach Running within 5m"})
			return
		case <-ticker.C:
			statuses, err := retryTransient(ctx, MaxTransientAttempts, func(ctx context.Context) (map[string]aliyun.InstanceStatus, error) {
				return m.client.DescribeInstanceStatuses(ctx, regionID, []string{instanceID})
			})
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				m.logger.Warn("start confirmation describe failed", "region", regionID, "instance", instanceID, "error", err)
				m.notify(ctx, notify.Message{RegionID: regionID, InstanceID: instanceID, EventType: notify.EventMonitorError, ErrorSummary: safeSummary(err), Detail: "start confirmation describe failed"})
				continue
			}
			status, ok := statuses[instanceID]
			if !ok {
				m.handleMissingStatus(ctx, regionID, instanceID)
				continue
			}
			current := strings.TrimSpace(status.Status)
			if current == statusRunning {
				m.logger.Info("instance running confirmed", "region", regionID, "instance", instanceID)
				m.notify(ctx, notify.Message{RegionID: regionID, InstanceID: instanceID, EventType: notify.EventRunningConfirmed, ErrorSummary: "instance reached Running"})
				return
			}
			m.logger.Info("waiting for instance running", "region", regionID, "instance", instanceID, "status", emptyFallback(current, "missing"))
		}
	}
}

func (m *Monitor) handleMissingStatus(ctx context.Context, regionID, instanceID string) {
	m.logger.Warn("instance status missing", "region", regionID, "instance", instanceID)
	m.notify(ctx, notify.Message{RegionID: regionID, InstanceID: instanceID, EventType: notify.EventMonitorError, ErrorSummary: "instance status missing"})
}

func (m *Monitor) notify(ctx context.Context, message notify.Message) {
	if err := m.notifier.Notify(ctx, message); err != nil && !errors.Is(err, context.Canceled) {
		m.logger.Warn("notify failed", "region", message.RegionID, "instance", message.InstanceID, "event", message.EventType, "error", err)
	}
}

func (m *Monitor) markInFlight(key string) bool {
	m.inFlightMu.Lock()
	defer m.inFlightMu.Unlock()
	if _, ok := m.inFlight[key]; ok {
		return false
	}
	m.inFlight[key] = struct{}{}
	return true
}

func (m *Monitor) clearInFlight(key string) {
	m.inFlightMu.Lock()
	defer m.inFlightMu.Unlock()
	delete(m.inFlight, key)
}

func (m *Monitor) pollInterval() time.Duration {
	if m == nil || m.cfg == nil || m.cfg.PollInterval <= 0 {
		return config.DefaultPollInterval
	}
	return m.cfg.PollInterval
}

func retryTransient[T any](ctx context.Context, maxAttempts int, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		value, err := fn(ctx)
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !aliyun.IsTransient(err) || attempt == maxAttempts {
			return zero, err
		}
		if err := sleepContext(ctx, retryDelay(attempt)); err != nil {
			return zero, err
		}
	}
	return zero, lastErr
}

func retryDelay(attempt int) time.Duration {
	base := time.Duration(1<<max(0, attempt-1)) * time.Second
	jitterLimit := max(int64(time.Millisecond), int64(base/2))
	return base + time.Duration(rand.Int63n(jitterLimit))
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func normalizeInstanceIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func instanceKey(regionID, instanceID string) string {
	return strings.TrimSpace(regionID) + "/" + strings.TrimSpace(instanceID)
}

func safeSummary(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%v", err)
}

func emptyFallback(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
