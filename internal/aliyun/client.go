// Package aliyun 提供面向监控逻辑的阿里云 ECS 客户端包装。
package aliyun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/models"
	ecs "github.com/alibabacloud-go/ecs-20140526/v7/client"
	"github.com/alibabacloud-go/tea/dara"
)

const (
	defaultAPITimeoutBudget = 15 * time.Second
	defaultConnectTimeout   = 5 * time.Second
	defaultReadTimeout      = defaultAPITimeoutBudget - defaultConnectTimeout
	describePageSize        = int32(50)
)

// InstanceStatus 是监控层需要的 ECS 实例状态快照。
type InstanceStatus struct {
	InstanceID string
	Status     string
}

// Client 暴露监控逻辑需要的 ECS 操作，不泄露 SDK request/response 类型。
type Client interface {
	DescribeInstanceStatuses(ctx context.Context, regionID string, instanceIDs []string) (map[string]InstanceStatus, error)
	StartInstance(ctx context.Context, regionID string, instanceID string) error
}

// ErrorKind 表示错误是否适合重试。
type ErrorKind string

const (
	ErrorKindTransient ErrorKind = "transient"
	ErrorKindBusiness  ErrorKind = "business"
)

// Error 是包装后的阿里云客户端错误。消息中只包含 code/status/requestID 等安全字段。
type Error struct {
	Kind       ErrorKind
	Operation  string
	RegionID   string
	InstanceID string
	Code       string
	StatusCode int
	RequestID  string
	Message    string
	Err        error
}

func (e *Error) Error() string {
	parts := []string{
		fmt.Sprintf("aliyun ecs %s failed", e.Operation),
		fmt.Sprintf("kind=%s", e.Kind),
	}
	if e.RegionID != "" {
		parts = append(parts, "region="+e.RegionID)
	}
	if e.InstanceID != "" {
		parts = append(parts, "instance="+e.InstanceID)
	}
	if e.Code != "" {
		parts = append(parts, "code="+e.Code)
	}
	if e.StatusCode != 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.StatusCode))
	}
	if e.RequestID != "" {
		parts = append(parts, "request="+e.RequestID)
	}
	if e.Message != "" {
		parts = append(parts, "message="+e.Message)
	}
	return strings.Join(parts, " ")
}

// IsTransient 返回错误是否为可重试的临时错误。
func IsTransient(err error) bool {
	var aliyunErr *Error
	if errors.As(err, &aliyunErr) {
		return aliyunErr.Kind == ErrorKindTransient
	}
	return false
}

type Config struct {
	AccessKeyID     string
	AccessKeySecret string
	RegionID        string
	Endpoint        string
	ConnectTimeout  time.Duration
	ReadTimeout     time.Duration
}

type ecsAPI interface {
	DescribeInstanceStatusWithOptions(*ecs.DescribeInstanceStatusRequest, *dara.RuntimeOptions) (*ecs.DescribeInstanceStatusResponse, error)
	StartInstanceWithOptions(*ecs.StartInstanceRequest, *dara.RuntimeOptions) (*ecs.StartInstanceResponse, error)
}

type ecsClient struct {
	api            ecsAPI
	regionID       string
	connectTimeout time.Duration
	readTimeout    time.Duration
}

// NewClient 使用 AK/SK 和 region 创建 ECS 客户端。endpoint 默认使用 ecs.<regionID>.aliyuncs.com。
func NewClient(config Config) (Client, error) {
	if strings.TrimSpace(config.AccessKeyID) == "" {
		return nil, errors.New("aliyun access key id is required")
	}
	if strings.TrimSpace(config.AccessKeySecret) == "" {
		return nil, errors.New("aliyun access key secret is required")
	}
	if strings.TrimSpace(config.RegionID) == "" {
		return nil, errors.New("aliyun region id is required")
	}

	connectTimeout := durationOrDefault(config.ConnectTimeout, defaultConnectTimeout)
	readTimeout := durationOrDefault(config.ReadTimeout, defaultReadTimeout)
	endpoint := strings.TrimSpace(config.Endpoint)
	if endpoint == "" {
		endpoint = EndpointForRegion(config.RegionID)
	}

	sdkConfig := &openapi.Config{
		AccessKeyId:     stringPtr(config.AccessKeyID),
		AccessKeySecret: stringPtr(config.AccessKeySecret),
		RegionId:        stringPtr(config.RegionID),
		Endpoint:        stringPtr(endpoint),
		ConnectTimeout:  intPtr(timeoutMilliseconds(connectTimeout)),
		ReadTimeout:     intPtr(timeoutMilliseconds(readTimeout)),
	}

	sdkClient, err := ecs.NewClient(sdkConfig)
	if err != nil {
		return nil, classifyError("new client", config.RegionID, "", err)
	}

	return &ecsClient{
		api:            sdkClient,
		regionID:       strings.TrimSpace(config.RegionID),
		connectTimeout: connectTimeout,
		readTimeout:    readTimeout,
	}, nil
}

func (c *ecsClient) DescribeInstanceStatuses(ctx context.Context, regionID string, instanceIDs []string) (map[string]InstanceStatus, error) {
	regionID = strings.TrimSpace(regionID)
	if regionID == "" {
		return nil, errors.New("aliyun region id is required")
	}
	if err := c.ensureRegion(regionID); err != nil {
		return nil, err
	}
	required, ordered, err := normalizeInstanceIDs(instanceIDs)
	if err != nil {
		return nil, err
	}

	statuses := make(map[string]InstanceStatus, len(required))
	var pageNumber int32 = 1
	for {
		if err := ctx.Err(); err != nil {
			return nil, classifyError("describe instance statuses", regionID, "", err)
		}

		request := &ecs.DescribeInstanceStatusRequest{
			RegionId:   stringPtr(regionID),
			InstanceId: toStringPtrs(ordered),
			PageNumber: int32Ptr(pageNumber),
			PageSize:   int32Ptr(describePageSize),
		}
		response, err := c.api.DescribeInstanceStatusWithOptions(request, c.runtimeOptions(ctx))
		if err != nil {
			return nil, classifyError("describe instance statuses", regionID, "", err)
		}

		if response == nil {
			return nil, classifyBusinessError("describe instance statuses", regionID, "", "EmptyResponse", "empty describe response")
		}
		body := response.GetBody()
		if body == nil {
			return nil, classifyBusinessError("describe instance statuses", regionID, "", "EmptyResponse", "empty describe response body")
		}
		statusList := body.GetInstanceStatuses()
		if statusList == nil {
			return nil, classifyBusinessError("describe instance statuses", regionID, "", "EmptyResponse", "empty instance statuses response")
		}
		for _, item := range statusList.GetInstanceStatus() {
			if item == nil || item.GetInstanceId() == nil || item.GetStatus() == nil {
				continue
			}
			instanceID := strings.TrimSpace(*item.GetInstanceId())
			if _, ok := required[instanceID]; !ok {
				continue
			}
			statuses[instanceID] = InstanceStatus{InstanceID: instanceID, Status: *item.GetStatus()}
		}

		pageSize := valueOrDefault(body.GetPageSize(), describePageSize)
		totalCount := valueOrDefault(body.GetTotalCount(), int32(len(statuses)))
		if pageNumber*pageSize >= totalCount || len(statuses) == len(required) {
			break
		}
		pageNumber++
	}

	if len(statuses) != len(required) {
		return nil, missingStatusesError(regionID, ordered, statuses)
	}
	return statuses, nil
}

func (c *ecsClient) StartInstance(ctx context.Context, regionID string, instanceID string) error {
	regionID = strings.TrimSpace(regionID)
	instanceID = strings.TrimSpace(instanceID)
	if regionID == "" {
		return errors.New("aliyun region id is required")
	}
	if instanceID == "" {
		return errors.New("aliyun instance id is required")
	}
	if err := c.ensureRegion(regionID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return classifyError("start instance", regionID, instanceID, err)
	}

	_, err := c.api.StartInstanceWithOptions(&ecs.StartInstanceRequest{InstanceId: stringPtr(instanceID)}, c.runtimeOptions(ctx))
	if err != nil {
		return classifyError("start instance", regionID, instanceID, err)
	}
	return nil
}

// EndpointForRegion 返回 ECS regional endpoint。
func EndpointForRegion(regionID string) string {
	return "ecs." + strings.TrimSpace(regionID) + ".aliyuncs.com"
}

func (c *ecsClient) ensureRegion(regionID string) error {
	if c.regionID == "" || regionID == c.regionID {
		return nil
	}
	return classifyBusinessError("validate region", regionID, "", "RegionMismatch", "client was constructed for a different region")
}

func (c *ecsClient) runtimeOptions(ctx context.Context) *dara.RuntimeOptions {
	connectTimeout := c.connectTimeout
	readTimeout := c.readTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			remaining = time.Millisecond
		}
		if remaining < connectTimeout+readTimeout {
			readTimeout = remaining - connectTimeout
			if readTimeout < time.Millisecond {
				connectTimeout = remaining / 2
				readTimeout = remaining - connectTimeout
			}
		}
	}
	// 保持请求预算与计划一致：默认总预算 15s；若 context 更短，则继续收紧。
	return &dara.RuntimeOptions{
		ConnectTimeout: intPtr(timeoutMilliseconds(connectTimeout)),
		ReadTimeout:    intPtr(timeoutMilliseconds(readTimeout)),
	}
}

type codedError interface {
	GetCode() *string
	GetMessage() *string
}

type statusCodedError interface {
	GetStatusCode() *int
}

type requestIDError interface {
	GetRequestId() *string
}

func classifyError(operation, regionID, instanceID string, err error) error {
	if err == nil {
		return nil
	}
	code := ""
	message := ""
	statusCode := 0
	requestID := ""

	var coded codedError
	if errors.As(err, &coded) {
		code = stringValue(coded.GetCode())
		message = stringValue(coded.GetMessage())
	}
	var statusCoded statusCodedError
	if errors.As(err, &statusCoded) {
		statusCode = intValue(statusCoded.GetStatusCode())
	}
	var requestErr requestIDError
	if errors.As(err, &requestErr) {
		requestID = stringValue(requestErr.GetRequestId())
	}
	if message == "" && code == "" {
		message = "request failed"
	}

	return &Error{
		Kind:       classifyKind(err, code, statusCode),
		Operation:  operation,
		RegionID:   regionID,
		InstanceID: instanceID,
		Code:       code,
		StatusCode: statusCode,
		RequestID:  requestID,
		Message:    sanitizeMessage(message),
	}
}

func classifyBusinessError(operation, regionID, instanceID, code, message string) error {
	return &Error{Kind: ErrorKindBusiness, Operation: operation, RegionID: regionID, InstanceID: instanceID, Code: code, Message: message}
}

func classifyKind(err error, code string, statusCode int) ErrorKind {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrorKindTransient
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ErrorKindTransient
	}
	if code == "IncorrectInstanceStatus" {
		return ErrorKindBusiness
	}
	if strings.HasPrefix(code, "Throttling") || code == "ServiceUnavailable" || code == "InternalError" {
		return ErrorKindTransient
	}
	if statusCode == 429 || statusCode >= 500 {
		return ErrorKindTransient
	}
	return ErrorKindBusiness
}

func missingStatusesError(regionID string, ordered []string, statuses map[string]InstanceStatus) error {
	missing := make([]error, 0)
	for _, instanceID := range ordered {
		if _, ok := statuses[instanceID]; !ok {
			missing = append(missing, classifyBusinessError("describe instance statuses", regionID, instanceID, "MissingInstanceStatus", "requested instance status missing from response"))
		}
	}
	return errors.Join(missing...)
}

func normalizeInstanceIDs(instanceIDs []string) (map[string]struct{}, []string, error) {
	seen := make(map[string]struct{}, len(instanceIDs))
	ordered := make([]string, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, nil, errors.New("aliyun instance id is required")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ordered = append(ordered, id)
	}
	if len(ordered) == 0 {
		return nil, nil, errors.New("aliyun instance ids are required")
	}
	return seen, ordered, nil
}

func durationOrDefault(value time.Duration, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func timeoutMilliseconds(value time.Duration) int {
	if value <= 0 {
		return 1
	}
	return max(1, int(value/time.Millisecond))
}

func toStringPtrs(values []string) []*string {
	result := make([]*string, 0, len(values))
	for _, value := range values {
		result = append(result, stringPtr(value))
	}
	return result
}

func sanitizeMessage(message string) string {
	if _, err := url.ParseRequestURI(message); err == nil {
		return "redacted url"
	}
	message = strings.ReplaceAll(message, "\n", " ")
	return strings.TrimSpace(message)
}

func stringPtr(value string) *string { return &value }
func intPtr(value int) *int          { return &value }
func int32Ptr(value int32) *int32    { return &value }

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func valueOrDefault(value *int32, fallback int32) int32 {
	if value == nil || *value <= 0 {
		return fallback
	}
	return *value
}
