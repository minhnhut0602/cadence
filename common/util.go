// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package common

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/robfig/cron"
	"github.com/uber/cadence/common/logging"
	"github.com/uber/cadence/common/metrics"
	"go.uber.org/yarpc/yarpcerrors"
	"golang.org/x/net/context"

	farm "github.com/dgryski/go-farm"
	"github.com/uber-common/bark"

	"math/rand"

	h "github.com/uber/cadence/.gen/go/history"
	m "github.com/uber/cadence/.gen/go/matching"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common/backoff"
)

const (
	retryPersistenceOperationInitialInterval    = 50 * time.Millisecond
	retryPersistenceOperationMaxInterval        = 10 * time.Second
	retryPersistenceOperationExpirationInterval = 30 * time.Second

	historyServiceOperationInitialInterval    = 50 * time.Millisecond
	historyServiceOperationMaxInterval        = 10 * time.Second
	historyServiceOperationExpirationInterval = 30 * time.Second

	matchingServiceOperationInitialInterval    = 1000 * time.Millisecond
	matchingServiceOperationMaxInterval        = 10 * time.Second
	matchingServiceOperationExpirationInterval = 30 * time.Second

	frontendServiceOperationInitialInterval    = 200 * time.Millisecond
	frontendServiceOperationMaxInterval        = 5 * time.Second
	frontendServiceOperationExpirationInterval = 15 * time.Second

	adminServiceOperationInitialInterval    = 200 * time.Millisecond
	adminServiceOperationMaxInterval        = 5 * time.Second
	adminServiceOperationExpirationInterval = 15 * time.Second

	retryKafkaOperationInitialInterval    = 50 * time.Millisecond
	retryKafkaOperationMaxInterval        = 10 * time.Second
	retryKafkaOperationExpirationInterval = 30 * time.Second

	// FailureReasonCompleteResultExceedsLimit is failureReason for complete result exceeds limit
	FailureReasonCompleteResultExceedsLimit = "COMPLETE_RESULT_EXCEEDS_LIMIT"
	// FailureReasonFailureDetailsExceedsLimit is failureReason for failure details exceeds limit
	FailureReasonFailureDetailsExceedsLimit = "FAILURE_DETAILS_EXCEEDS_LIMIT"
	// FailureReasonCancelDetailsExceedsLimit is failureReason for cancel details exceeds limit
	FailureReasonCancelDetailsExceedsLimit = "CANCEL_DETAILS_EXCEEDS_LIMIT"
	//FailureReasonHeartbeatExceedsLimit is failureReason for heartbeat exceeds limit
	FailureReasonHeartbeatExceedsLimit = "HEARTBEAT_EXCEEDS_LIMIT"
	// FailureReasonDecisionBlobSizeExceedsLimit is the failureReason for decision blob exceeds size limit
	FailureReasonDecisionBlobSizeExceedsLimit = "DECISION_BLOB_SIZE_EXCEEDS_LIMIT"
	// TerminateReasonSizeExceedsLimit is reason to terminate workflow when history size exceed limit
	TerminateReasonSizeExceedsLimit = "HISTORY_SIZE_EXCEEDS_LIMIT"
)

var (
	// ErrBlobSizeExceedsLimit is error for event blob size exceeds limit
	ErrBlobSizeExceedsLimit = &workflow.BadRequestError{Message: "Blob data size exceeds limit."}
)

// AwaitWaitGroup calls Wait on the given wait
// Returns true if the Wait() call succeeded before the timeout
// Returns false if the Wait() did not return before the timeout
func AwaitWaitGroup(wg *sync.WaitGroup, timeout time.Duration) bool {

	doneC := make(chan struct{})

	go func() {
		wg.Wait()
		close(doneC)
	}()

	select {
	case <-doneC:
		return true
	case <-time.After(timeout):
		return false
	}
}

// AddSecondsToBaseTime - Gets the UnixNano with given duration and base time.
func AddSecondsToBaseTime(baseTimeInNanoSec int64, durationInSeconds int64) int64 {
	timeOut := time.Duration(durationInSeconds) * time.Second
	return time.Unix(0, baseTimeInNanoSec).Add(timeOut).UnixNano()
}

// CreatePersistanceRetryPolicy creates a retry policy for persistence layer operations
func CreatePersistanceRetryPolicy() backoff.RetryPolicy {
	policy := backoff.NewExponentialRetryPolicy(retryPersistenceOperationInitialInterval)
	policy.SetMaximumInterval(retryPersistenceOperationMaxInterval)
	policy.SetExpirationInterval(retryPersistenceOperationExpirationInterval)

	return policy
}

// CreateHistoryServiceRetryPolicy creates a retry policy for calls to history service
func CreateHistoryServiceRetryPolicy() backoff.RetryPolicy {
	policy := backoff.NewExponentialRetryPolicy(historyServiceOperationInitialInterval)
	policy.SetMaximumInterval(historyServiceOperationMaxInterval)
	policy.SetExpirationInterval(historyServiceOperationExpirationInterval)

	return policy
}

// CreateMatchingServiceRetryPolicy creates a retry policy for calls to matching service
func CreateMatchingServiceRetryPolicy() backoff.RetryPolicy {
	policy := backoff.NewExponentialRetryPolicy(matchingServiceOperationInitialInterval)
	policy.SetMaximumInterval(matchingServiceOperationMaxInterval)
	policy.SetExpirationInterval(matchingServiceOperationExpirationInterval)

	return policy
}

// CreateFrontendServiceRetryPolicy creates a retry policy for calls to frontend service
func CreateFrontendServiceRetryPolicy() backoff.RetryPolicy {
	policy := backoff.NewExponentialRetryPolicy(frontendServiceOperationInitialInterval)
	policy.SetMaximumInterval(frontendServiceOperationMaxInterval)
	policy.SetExpirationInterval(frontendServiceOperationExpirationInterval)

	return policy
}

// CreateAdminServiceRetryPolicy creates a retry policy for calls to matching service
func CreateAdminServiceRetryPolicy() backoff.RetryPolicy {
	policy := backoff.NewExponentialRetryPolicy(adminServiceOperationInitialInterval)
	policy.SetMaximumInterval(adminServiceOperationMaxInterval)
	policy.SetExpirationInterval(adminServiceOperationExpirationInterval)

	return policy
}

// CreateKafkaOperationRetryPolicy creates a retry policy for kafka operation
func CreateKafkaOperationRetryPolicy() backoff.RetryPolicy {
	policy := backoff.NewExponentialRetryPolicy(retryKafkaOperationInitialInterval)
	policy.SetMaximumInterval(retryKafkaOperationMaxInterval)
	policy.SetExpirationInterval(retryKafkaOperationExpirationInterval)

	return policy
}

// IsPersistenceTransientError checks if the error is a transient persistence error
func IsPersistenceTransientError(err error) bool {
	switch err.(type) {
	case *workflow.InternalServiceError, *workflow.ServiceBusyError:
		return true
	}

	return false
}

// IsKafkaTransientError check if the error is a transient kafka error
func IsKafkaTransientError(err error) bool {
	return true
}

// IsServiceTransientError checks if the error is a retryable error.
func IsServiceTransientError(err error) bool {
	return !IsServiceNonRetryableError(err)
}

// IsServiceNonRetryableError checks if the error is a non retryable error.
func IsServiceNonRetryableError(err error) bool {
	switch err.(type) {
	case *workflow.EntityNotExistsError:
		return true
	case *workflow.BadRequestError:
		return true
	case *workflow.DomainNotActiveError:
		return true
	case *workflow.CancellationAlreadyRequestedError:
		return true
	case *yarpcerrors.Status:
		rpcErr := err.(*yarpcerrors.Status)
		if rpcErr.Code() != yarpcerrors.CodeDeadlineExceeded {
			return true
		}
		return false
	}

	return false
}

// IsWhitelistServiceTransientError checks if the error is a transient error.
func IsWhitelistServiceTransientError(err error) bool {
	if err == context.DeadlineExceeded {
		return true
	}

	switch err.(type) {
	case *workflow.InternalServiceError:
		return true
	case *workflow.ServiceBusyError:
		return true
	case *workflow.LimitExceededError:
		return true
	case *h.ShardOwnershipLostError:
		return true
	case *yarpcerrors.Status:
		// We only selectively retry the following yarpc errors client can safe retry with a backoff
		if yarpcerrors.IsDeadlineExceeded(err) ||
			yarpcerrors.IsUnavailable(err) ||
			yarpcerrors.IsUnknown(err) ||
			yarpcerrors.IsInternal(err) {
			return true
		}
		return false
	}

	return false
}

// WorkflowIDToHistoryShard is used to map workflowID to a shardID
func WorkflowIDToHistoryShard(workflowID string, numberOfShards int) int {
	hash := farm.Fingerprint32([]byte(workflowID))
	return int(hash % uint32(numberOfShards))
}

// PrettyPrintHistory prints history in human readable format
func PrettyPrintHistory(history *workflow.History, logger bark.Logger) {
	data, err := json.MarshalIndent(history, "", "    ")

	if err != nil {
		logger.Errorf("Error serializing history: %v\n", err)
	}

	logger.Info("******************************************")
	logger.Infof("History: %v", string(data))
	logger.Info("******************************************")
}

// IsValidContext checks that the thrift context is not expired on cancelled.
// Returns nil if the context is still valid. Otherwise, returns the result of
// ctx.Err()
func IsValidContext(ctx context.Context) error {
	ch := ctx.Done()
	if ch != nil {
		select {
		case <-ch:
			return ctx.Err()
		default:
			return nil
		}
	}
	return nil
}

// GenerateRandomString is used for generate test string
func GenerateRandomString(n int) string {
	rand.Seed(time.Now().UnixNano())
	letterRunes := []rune("random")
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

// CreateMatchingPollForDecisionTaskResponse create response for matching's PollForDecisionTask
func CreateMatchingPollForDecisionTaskResponse(historyResponse *h.RecordDecisionTaskStartedResponse, workflowExecution *workflow.WorkflowExecution, token []byte) *m.PollForDecisionTaskResponse {
	matchingResp := &m.PollForDecisionTaskResponse{
		WorkflowExecution:         workflowExecution,
		TaskToken:                 token,
		Attempt:                   Int64Ptr(historyResponse.GetAttempt()),
		WorkflowType:              historyResponse.WorkflowType,
		StartedEventId:            historyResponse.StartedEventId,
		StickyExecutionEnabled:    historyResponse.StickyExecutionEnabled,
		NextEventId:               historyResponse.NextEventId,
		DecisionInfo:              historyResponse.DecisionInfo,
		WorkflowExecutionTaskList: historyResponse.WorkflowExecutionTaskList,
		EventStoreVersion:         historyResponse.EventStoreVersion,
		BranchToken:               historyResponse.BranchToken,
	}
	if historyResponse.GetPreviousStartedEventId() != EmptyEventID {
		matchingResp.PreviousStartedEventId = historyResponse.PreviousStartedEventId
	}
	return matchingResp
}

// MinInt32 return smaller one of two inputs int32
func MinInt32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

// ValidateRetryPolicy validates a retry policy
func ValidateRetryPolicy(policy *workflow.RetryPolicy) error {
	if policy == nil {
		// nil policy is valid which means no retry
		return nil
	}
	if policy.GetInitialIntervalInSeconds() <= 0 {
		return &workflow.BadRequestError{Message: "InitialIntervalInSeconds must be greater than 0 on retry policy."}
	}
	if policy.GetBackoffCoefficient() < 1 {
		return &workflow.BadRequestError{Message: "BackoffCoefficient cannot be less than 1 on retry policy."}
	}
	if policy.GetMaximumIntervalInSeconds() < 0 {
		return &workflow.BadRequestError{Message: "MaximumIntervalInSeconds cannot be less than 0 on retry policy."}
	}
	if policy.GetMaximumIntervalInSeconds() > 0 && policy.GetMaximumIntervalInSeconds() < policy.GetInitialIntervalInSeconds() {
		return &workflow.BadRequestError{Message: "MaximumIntervalInSeconds cannot be less than InitialIntervalInSeconds on retry policy."}
	}
	if policy.GetMaximumAttempts() < 0 {
		return &workflow.BadRequestError{Message: "MaximumAttempts cannot be less than 0 on retry policy."}
	}
	if policy.GetExpirationIntervalInSeconds() < 0 {
		return &workflow.BadRequestError{Message: "ExpirationIntervalInSeconds cannot be less than 0 on retry policy."}
	}
	if policy.GetMaximumAttempts() == 0 && policy.GetExpirationIntervalInSeconds() == 0 {
		return &workflow.BadRequestError{Message: "MaximumAttempts and ExpirationIntervalInSeconds are both 0. At least one of them must be specified."}
	}
	return nil
}

// ValidateCronSchedule validates a cron schedule spec
func ValidateCronSchedule(cronSchedule string) error {
	if cronSchedule == "" {
		return nil
	}
	if _, err := cron.Parse(cronSchedule); err != nil {
		return &workflow.BadRequestError{Message: "Invalid CronSchedule."}
	}
	return nil
}

// CreateHistoryStartWorkflowRequest create a start workflow request for history
func CreateHistoryStartWorkflowRequest(domainID string, startRequest *workflow.StartWorkflowExecutionRequest) *h.StartWorkflowExecutionRequest {
	histRequest := &h.StartWorkflowExecutionRequest{
		DomainUUID:   StringPtr(domainID),
		StartRequest: startRequest,
	}
	if startRequest.RetryPolicy != nil && startRequest.RetryPolicy.GetExpirationIntervalInSeconds() > 0 {
		expirationInSeconds := startRequest.RetryPolicy.GetExpirationIntervalInSeconds()
		deadline := time.Now().Add(time.Second * time.Duration(expirationInSeconds))
		histRequest.ExpirationTimestamp = Int64Ptr(deadline.Round(time.Millisecond).UnixNano())
	}
	return histRequest
}

// CheckEventBlobSizeLimit checks if a blob data exceeds limits. It logs a warning if it exceeds warnLimit,
// and return ErrBlobSizeExceedsLimit if it exceeds errorLimit.
func CheckEventBlobSizeLimit(actualSize, warnLimit, errorLimit int, domainID, workflowID, runID string, metricsClient metrics.Client, scope int, logger bark.Logger) error {
	if metricsClient != nil {
		metricsClient.RecordTimer(
			scope,
			metrics.EventBlobSize,
			time.Duration(actualSize),
		)
	}

	if actualSize > warnLimit {
		if logger != nil {
			logger.WithFields(bark.Fields{
				logging.TagDomainID:            domainID,
				logging.TagWorkflowExecutionID: workflowID,
				logging.TagWorkflowRunID:       runID,
				logging.TagSize:                actualSize,
			}).Warn("Blob size exceeds limit.")
		}

		if actualSize > errorLimit {
			return ErrBlobSizeExceedsLimit
		}
	}
	return nil
}
