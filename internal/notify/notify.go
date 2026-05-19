package notify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Notifier 发送监控事件通知。
type Notifier interface {
	Notify(ctx context.Context, message Message) error
}

// Message 描述一次需要通知的监控事件。
type Message struct {
	RegionID      string
	InstanceID    string
	EventType     EventType
	ErrorSummary  string
	Detail        string
	MaxTextLength int
}

// EventType 描述通知事件类型。
type EventType string

const (
	EventStoppedDetected  EventType = "stopped_detected"
	EventStartAccepted    EventType = "start_accepted"
	EventStartFailed      EventType = "start_failed"
	EventRunningConfirmed EventType = "running_confirmed"
	EventStartTimeout     EventType = "start_timeout"
	EventMonitorError     EventType = "monitor_error"
)

// NoopNotifier 什么都不做。
type NoopNotifier struct{}

// Notify 实现 Notifier。
func (NoopNotifier) Notify(context.Context, Message) error { return nil }

// FormatMessage 把监控事件格式化为 Telegram 可读文本。
func FormatMessage(message Message) string {
	limit := defaultTelegramLimit
	if message.MaxTextLength > 0 && message.MaxTextLength < limit {
		limit = message.MaxTextLength
	}
	return formatMessage(message, limit)
}

// FormatStoppedDetected 构造 stopped 事件文本。
func FormatStoppedDetected(regionID, instanceID, summary string) string {
	return FormatMessage(Message{RegionID: regionID, InstanceID: instanceID, EventType: EventStoppedDetected, ErrorSummary: summary})
}

// FormatStartAccepted 构造启动接受事件文本。
func FormatStartAccepted(regionID, instanceID, summary string) string {
	return FormatMessage(Message{RegionID: regionID, InstanceID: instanceID, EventType: EventStartAccepted, ErrorSummary: summary})
}

// FormatStartFailed 构造启动失败事件文本。
func FormatStartFailed(regionID, instanceID, summary string) string {
	return FormatMessage(Message{RegionID: regionID, InstanceID: instanceID, EventType: EventStartFailed, ErrorSummary: summary})
}

// FormatRunningConfirmed 构造 running 确认事件文本。
func FormatRunningConfirmed(regionID, instanceID, summary string) string {
	return FormatMessage(Message{RegionID: regionID, InstanceID: instanceID, EventType: EventRunningConfirmed, ErrorSummary: summary})
}

// FormatStartTimeout 构造超时事件文本。
func FormatStartTimeout(regionID, instanceID, summary string) string {
	return FormatMessage(Message{RegionID: regionID, InstanceID: instanceID, EventType: EventStartTimeout, ErrorSummary: summary})
}

// FormatMonitorError 构造监控错误事件文本。
func FormatMonitorError(regionID, instanceID, summary string) string {
	return FormatMessage(Message{RegionID: regionID, InstanceID: instanceID, EventType: EventMonitorError, ErrorSummary: summary})
}

func formatMessage(message Message, limit int) string {
	if limit <= 0 {
		limit = defaultTelegramLimit
	}
	text := messageText(message)
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	if limit <= len(truncationSuffix) {
		return runeSlice(truncationSuffix, limit)
	}
	return runeSlice(text, limit-utf8.RuneCountInString(truncationSuffix)) + truncationSuffix
}

func messageText(message Message) string {
	lines := []string{
		fmt.Sprintf("event: %s", message.EventType),
		fmt.Sprintf("region: %s", emptyFallback(message.RegionID)),
		fmt.Sprintf("instance: %s", emptyFallback(message.InstanceID)),
	}
	if message.ErrorSummary != "" {
		lines = append(lines, fmt.Sprintf("error: %s", message.ErrorSummary))
	}
	if message.Detail != "" {
		lines = append(lines, fmt.Sprintf("detail: %s", message.Detail))
	}
	return joinLines(lines)
}

func emptyFallback(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	var builder strings.Builder
	for i, line := range lines {
		if i > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(line)
	}
	return builder.String()
}

func runeSlice(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	index := 0
	for i := 0; i < len(text) && limit > 0; limit-- {
		_, size := utf8.DecodeRuneInString(text[i:])
		index = i + size
		i = index
	}
	return text[:index]
}

const (
	defaultTelegramLimit = 4096
	truncationSuffix     = "… [truncated]"
)

// NewNoop 返回 noop 通知器。
func NewNoop() Notifier { return NoopNotifier{} }

// TelegramConfig 控制 Telegram 通知器。
type TelegramConfig struct {
	BotToken string
	ChatID   string
	Client   *http.Client
	BaseURL  string
	Timeout  time.Duration
}
