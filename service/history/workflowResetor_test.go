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

package history

import (
	"context"
	"os"
	"testing"

	"fmt"
	"math"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/uber-common/bark"
	"github.com/uber-go/tally"
	h "github.com/uber/cadence/.gen/go/history"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/client"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/messaging"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/mocks"
	p "github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/service"
)

type (
	resetorSuite struct {
		suite.Suite
		// override suite.Suite.Assertions with require.Assertions; this means that s.NotNil(nil) will stop the test,
		// not merely log an error
		*require.Assertions
		historyEngine       *historyEngineImpl
		mockMatchingClient  *mocks.MatchingClient
		mockHistoryClient   *mocks.HistoryClient
		mockMetadataMgr     *mocks.MetadataManager
		mockVisibilityMgr   *mocks.VisibilityManager
		mockExecutionMgr    *mocks.ExecutionManager
		mockHistoryMgr      *mocks.HistoryManager
		mockHistoryV2Mgr    *mocks.HistoryV2Manager
		mockShardManager    *mocks.ShardManager
		mockClusterMetadata *mocks.ClusterMetadata
		mockProducer        *mocks.KafkaProducer
		mockMessagingClient messaging.Client
		mockService         service.Service
		mockDomainCache     *cache.DomainCacheMock
		mockArchivalClient  *mocks.ArchivalClient
		mockClientBean      *client.MockClientBean

		shardClosedCh chan int
		config        *Config
		logger        bark.Logger
	}
)

func TestWorkflowResetorSuite(t *testing.T) {
	s := new(resetorSuite)
	suite.Run(t, s)
}

func (s *resetorSuite) SetupSuite() {
	if testing.Verbose() {
		log.SetOutput(os.Stdout)
	}

	l := log.New()
	l.Level = log.DebugLevel
	s.logger = bark.NewLoggerFromLogrus(l)
	s.config = NewDynamicConfigForEventsV2Test()
}

func (s *resetorSuite) TearDownSuite() {
}

func (s *resetorSuite) SetupTest() {
	// Have to define our overridden assertions in the test setup. If we did it earlier, s.T() will return nil
	s.Assertions = require.New(s.T())

	shardID := 0
	s.mockMatchingClient = &mocks.MatchingClient{}
	s.mockHistoryClient = &mocks.HistoryClient{}
	s.mockMetadataMgr = &mocks.MetadataManager{}
	s.mockVisibilityMgr = &mocks.VisibilityManager{}
	s.mockExecutionMgr = &mocks.ExecutionManager{}
	s.mockHistoryMgr = &mocks.HistoryManager{}
	s.mockHistoryV2Mgr = &mocks.HistoryV2Manager{}
	s.mockShardManager = &mocks.ShardManager{}
	s.mockClusterMetadata = &mocks.ClusterMetadata{}
	s.mockProducer = &mocks.KafkaProducer{}
	s.shardClosedCh = make(chan int, 100)
	metricsClient := metrics.NewClient(tally.NoopScope, metrics.History)
	s.mockMessagingClient = mocks.NewMockMessagingClient(s.mockProducer, nil)
	s.mockClientBean = &client.MockClientBean{}
	s.mockService = service.NewTestService(s.mockClusterMetadata, s.mockMessagingClient, metricsClient, s.mockClientBean, s.logger)
	s.mockClusterMetadata.On("GetCurrentClusterName").Return(cluster.TestCurrentClusterName)
	s.mockClusterMetadata.On("GetAllClusterFailoverVersions").Return(cluster.TestSingleDCAllClusterFailoverVersions)
	s.mockClusterMetadata.On("IsGlobalDomainEnabled").Return(false)
	s.mockDomainCache = &cache.DomainCacheMock{}
	s.mockArchivalClient = &mocks.ArchivalClient{}

	mockShard := &shardContextImpl{
		service:                   s.mockService,
		shardInfo:                 &p.ShardInfo{ShardID: shardID, RangeID: 1, TransferAckLevel: 0},
		transferSequenceNumber:    1,
		executionManager:          s.mockExecutionMgr,
		historyMgr:                s.mockHistoryMgr,
		historyV2Mgr:              s.mockHistoryV2Mgr,
		domainCache:               s.mockDomainCache,
		shardManager:              s.mockShardManager,
		maxTransferSequenceNumber: 100000,
		closeCh:                   s.shardClosedCh,
		config:                    s.config,
		logger:                    s.logger,
		metricsClient:             metrics.NewClient(tally.NoopScope, metrics.History),
	}

	historyCache := newHistoryCache(mockShard)
	h := &historyEngineImpl{
		currentClusterName: mockShard.GetService().GetClusterMetadata().GetCurrentClusterName(),
		shard:              mockShard,
		executionManager:   s.mockExecutionMgr,
		historyMgr:         s.mockHistoryMgr,
		historyV2Mgr:       s.mockHistoryV2Mgr,
		historyCache:       historyCache,
		logger:             s.logger,
		metricsClient:      metrics.NewClient(tally.NoopScope, metrics.History),
		tokenSerializer:    common.NewJSONTaskTokenSerializer(),
		config:             s.config,
		archivalClient:     s.mockArchivalClient,
	}
	h.txProcessor = newTransferQueueProcessor(mockShard, h, s.mockVisibilityMgr, s.mockProducer, s.mockMatchingClient, s.mockHistoryClient, s.logger)
	h.timerProcessor = newTimerQueueProcessor(mockShard, h, s.mockMatchingClient, s.mockProducer, s.logger)
	h.resetor = newWorkflowResetor(h)
	s.historyEngine = h
}

func (s *resetorSuite) TearDownTest() {
	s.mockMatchingClient.AssertExpectations(s.T())
	s.mockExecutionMgr.AssertExpectations(s.T())
	s.mockHistoryMgr.AssertExpectations(s.T())
	s.mockHistoryV2Mgr.AssertExpectations(s.T())
	s.mockShardManager.AssertExpectations(s.T())
	s.mockVisibilityMgr.AssertExpectations(s.T())
	s.mockClusterMetadata.AssertExpectations(s.T())
	s.mockProducer.AssertExpectations(s.T())
	s.mockClientBean.AssertExpectations(s.T())
	s.mockArchivalClient.AssertExpectations(s.T())
}

func (s *resetorSuite) TestResetWorkflowExecution_NoReplication() {
	testDomainEntry := cache.NewDomainCacheEntryForTest(&p.DomainInfo{ID: validDomainID}, &p.DomainConfig{Retention: 1})
	s.mockDomainCache.On("GetDomainByID", mock.Anything).Return(testDomainEntry, nil)
	s.mockDomainCache.On("GetDomain", mock.Anything).Return(testDomainEntry, nil)

	request := &h.ResetWorkflowExecutionRequest{}
	_, err := s.historyEngine.ResetWorkflowExecution(context.Background(), request)
	s.EqualError(err, "BadRequestError{Message: Missing domain UUID.}")

	domainID := validDomainID
	request.DomainUUID = &domainID
	request.ResetRequest = &workflow.ResetWorkflowExecutionRequest{}
	_, err = s.historyEngine.ResetWorkflowExecution(context.Background(), request)
	s.EqualError(err, "BadRequestError{Message: Require workflowId and runId.}")

	wid := "wId"
	wfType := "wfType"
	taskListName := "taskList"
	forkRunID := uuid.New().String()
	currRunID := uuid.New().String()
	we := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(wid),
		RunId:      common.StringPtr(forkRunID),
	}
	request.ResetRequest = &workflow.ResetWorkflowExecutionRequest{
		Domain:                common.StringPtr("testDomainName"),
		WorkflowExecution:     &we,
		Reason:                common.StringPtr("test reset"),
		DecisionFinishEventId: common.Int64Ptr(29),
		RequestId:             common.StringPtr(uuid.New().String()),
	}

	forkGwmsRequest := &p.GetWorkflowExecutionRequest{
		DomainID: domainID,
		Execution: workflow.WorkflowExecution{
			WorkflowId: common.StringPtr(wid),
			RunId:      common.StringPtr(forkRunID),
		},
	}

	timerFiredID := "timerID0"
	timerUnfiredID1 := "timerID1"
	timerUnfiredID2 := "timerID2"
	timerAfterReset := "timerID3"
	actIDCompleted1 := "actID0"
	actIDCompleted2 := "actID1"
	actIDStarted1 := "actID2"
	actIDNotStarted := "actID3"
	actIDStarted2 := "actID4"
	signalName1 :=
		"sig1"
	signalName2 := "sig2"
	forkBranchToken := []byte("forkBranchToken")
	forkExeInfo := &p.WorkflowExecutionInfo{
		DomainID:          domainID,
		WorkflowID:        wid,
		WorkflowTypeName:  wfType,
		TaskList:          taskListName,
		RunID:             forkRunID,
		EventStoreVersion: p.EventStoreVersionV2,
		BranchToken:       forkBranchToken,
		NextEventID:       34,
	}
	forkGwmsResponse := &p.GetWorkflowExecutionResponse{State: &p.WorkflowMutableState{
		ExecutionInfo: forkExeInfo,
	}}

	currGwmsRequest := &p.GetWorkflowExecutionRequest{
		DomainID: domainID,
		Execution: workflow.WorkflowExecution{
			WorkflowId: common.StringPtr(wid),
			RunId:      common.StringPtr(currRunID),
		},
	}
	currExeInfo := &p.WorkflowExecutionInfo{
		DomainID:         domainID,
		WorkflowID:       wid,
		WorkflowTypeName: wfType,
		TaskList:         taskListName,
		RunID:            currRunID,
		NextEventID:      common.FirstEventID,
	}
	compareCurrExeInfo := copyWorkflowExecutionInfo(currExeInfo)
	currGwmsResponse := &p.GetWorkflowExecutionResponse{State: &p.WorkflowMutableState{
		ExecutionInfo: currExeInfo,
	}}

	gcurResponse := &p.GetCurrentExecutionResponse{
		RunID: currRunID,
	}

	readHistoryReq := &p.ReadHistoryBranchRequest{
		BranchToken:   forkBranchToken,
		MinEventID:    common.FirstEventID,
		MaxEventID:    int64(34),
		PageSize:      defaultHistoryPageSize,
		NextPageToken: nil,
	}

	taskList := &workflow.TaskList{
		Name: common.StringPtr(taskListName),
	}
	readHistoryResp := &p.ReadHistoryBranchByBatchResponse{
		NextPageToken:    nil,
		Size:             1000,
		LastFirstEventID: int64(31),
		History: []*workflow.History{
			&workflow.History{
				Events: []*workflow.HistoryEvent{
					&workflow.HistoryEvent{
						EventId:   common.Int64Ptr(1),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeWorkflowExecutionStarted),
						WorkflowExecutionStartedEventAttributes: &workflow.WorkflowExecutionStartedEventAttributes{
							WorkflowType: &workflow.WorkflowType{
								Name: common.StringPtr(wfType),
							},
							TaskList:                            taskList,
							Input:                               []byte("testInput"),
							ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(100),
							TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(200),
						},
					},
					{
						EventId:   common.Int64Ptr(2),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskScheduled),
						DecisionTaskScheduledEventAttributes: &workflow.DecisionTaskScheduledEventAttributes{
							TaskList:                   taskList,
							StartToCloseTimeoutSeconds: common.Int32Ptr(100),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(3),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskStarted),
						DecisionTaskStartedEventAttributes: &workflow.DecisionTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(2),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(4),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskCompleted),
						DecisionTaskCompletedEventAttributes: &workflow.DecisionTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(2),
							StartedEventId:   common.Int64Ptr(3),
						},
					},
					{
						EventId:   common.Int64Ptr(5),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeMarkerRecorded),
						MarkerRecordedEventAttributes: &workflow.MarkerRecordedEventAttributes{
							MarkerName:                   common.StringPtr("Version"),
							Details:                      []byte("details"),
							DecisionTaskCompletedEventId: common.Int64Ptr(4),
						},
					},
					{
						EventId:   common.Int64Ptr(6),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskScheduled),
						ActivityTaskScheduledEventAttributes: &workflow.ActivityTaskScheduledEventAttributes{
							ActivityId: common.StringPtr(actIDCompleted1),
							ActivityType: &workflow.ActivityType{
								Name: common.StringPtr("actType0"),
							},
							TaskList:                      taskList,
							ScheduleToCloseTimeoutSeconds: common.Int32Ptr(1000),
							ScheduleToStartTimeoutSeconds: common.Int32Ptr(2000),
							StartToCloseTimeoutSeconds:    common.Int32Ptr(3000),
							HeartbeatTimeoutSeconds:       common.Int32Ptr(4000),
							DecisionTaskCompletedEventId:  common.Int64Ptr(4),
						},
					},
					{
						EventId:   common.Int64Ptr(7),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeTimerStarted),
						TimerStartedEventAttributes: &workflow.TimerStartedEventAttributes{
							TimerId:                      common.StringPtr(timerFiredID),
							StartToFireTimeoutSeconds:    common.Int64Ptr(2),
							DecisionTaskCompletedEventId: common.Int64Ptr(4),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(8),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskStarted),
						ActivityTaskStartedEventAttributes: &workflow.ActivityTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(6),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(9),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskCompleted),
						ActivityTaskCompletedEventAttributes: &workflow.ActivityTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(6),
							StartedEventId:   common.Int64Ptr(8),
						},
					},
					{
						EventId:   common.Int64Ptr(10),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskScheduled),
						DecisionTaskScheduledEventAttributes: &workflow.DecisionTaskScheduledEventAttributes{
							TaskList:                   taskList,
							StartToCloseTimeoutSeconds: common.Int32Ptr(100),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(11),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskStarted),
						DecisionTaskStartedEventAttributes: &workflow.DecisionTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(10),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(12),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskCompleted),
						DecisionTaskCompletedEventAttributes: &workflow.DecisionTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(10),
							StartedEventId:   common.Int64Ptr(11),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(13),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeTimerFired),
						TimerFiredEventAttributes: &workflow.TimerFiredEventAttributes{
							TimerId: common.StringPtr(timerFiredID),
						},
					},
					{
						EventId:   common.Int64Ptr(14),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskScheduled),
						DecisionTaskScheduledEventAttributes: &workflow.DecisionTaskScheduledEventAttributes{
							TaskList:                   taskList,
							StartToCloseTimeoutSeconds: common.Int32Ptr(100),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(15),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskStarted),
						DecisionTaskStartedEventAttributes: &workflow.DecisionTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(14),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(16),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskCompleted),
						DecisionTaskCompletedEventAttributes: &workflow.DecisionTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(14),
							StartedEventId:   common.Int64Ptr(15),
						},
					},
					{
						EventId:   common.Int64Ptr(17),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskScheduled),
						ActivityTaskScheduledEventAttributes: &workflow.ActivityTaskScheduledEventAttributes{
							ActivityId: common.StringPtr(actIDStarted1),
							ActivityType: &workflow.ActivityType{
								Name: common.StringPtr("actType1"),
							},
							TaskList:                      taskList,
							ScheduleToCloseTimeoutSeconds: common.Int32Ptr(1000),
							ScheduleToStartTimeoutSeconds: common.Int32Ptr(2000),
							StartToCloseTimeoutSeconds:    common.Int32Ptr(3000),
							HeartbeatTimeoutSeconds:       common.Int32Ptr(4000),
							DecisionTaskCompletedEventId:  common.Int64Ptr(16),
							RetryPolicy: &workflow.RetryPolicy{
								InitialIntervalInSeconds:    common.Int32Ptr(1),
								BackoffCoefficient:          common.Float64Ptr(0.2),
								MaximumAttempts:             common.Int32Ptr(10),
								MaximumIntervalInSeconds:    common.Int32Ptr(1000),
								ExpirationIntervalInSeconds: common.Int32Ptr(math.MaxInt32),
							},
						},
					},
					{
						EventId:   common.Int64Ptr(18),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskScheduled),
						ActivityTaskScheduledEventAttributes: &workflow.ActivityTaskScheduledEventAttributes{
							ActivityId: common.StringPtr(actIDNotStarted),
							ActivityType: &workflow.ActivityType{
								Name: common.StringPtr("actType2"),
							},
							TaskList:                      taskList,
							ScheduleToCloseTimeoutSeconds: common.Int32Ptr(1000),
							ScheduleToStartTimeoutSeconds: common.Int32Ptr(2000),
							StartToCloseTimeoutSeconds:    common.Int32Ptr(3000),
							HeartbeatTimeoutSeconds:       common.Int32Ptr(4000),
							DecisionTaskCompletedEventId:  common.Int64Ptr(16),
						},
					},
					{
						EventId:   common.Int64Ptr(19),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeTimerStarted),
						TimerStartedEventAttributes: &workflow.TimerStartedEventAttributes{
							TimerId:                      common.StringPtr(timerUnfiredID1),
							StartToFireTimeoutSeconds:    common.Int64Ptr(4),
							DecisionTaskCompletedEventId: common.Int64Ptr(16),
						},
					},
					{
						EventId:   common.Int64Ptr(20),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeTimerStarted),
						TimerStartedEventAttributes: &workflow.TimerStartedEventAttributes{
							TimerId:                      common.StringPtr(timerUnfiredID2),
							StartToFireTimeoutSeconds:    common.Int64Ptr(8),
							DecisionTaskCompletedEventId: common.Int64Ptr(16),
						},
					},
					{
						EventId:   common.Int64Ptr(21),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskScheduled),
						ActivityTaskScheduledEventAttributes: &workflow.ActivityTaskScheduledEventAttributes{
							ActivityId: common.StringPtr(actIDCompleted2),
							ActivityType: &workflow.ActivityType{
								Name: common.StringPtr("actType2"),
							},
							TaskList:                      taskList,
							ScheduleToCloseTimeoutSeconds: common.Int32Ptr(1000),
							ScheduleToStartTimeoutSeconds: common.Int32Ptr(2000),
							StartToCloseTimeoutSeconds:    common.Int32Ptr(3000),
							HeartbeatTimeoutSeconds:       common.Int32Ptr(4000),
							DecisionTaskCompletedEventId:  common.Int64Ptr(16),
						},
					},
					{
						EventId:   common.Int64Ptr(22),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskScheduled),
						ActivityTaskScheduledEventAttributes: &workflow.ActivityTaskScheduledEventAttributes{
							ActivityId: common.StringPtr(actIDStarted2),
							ActivityType: &workflow.ActivityType{
								Name: common.StringPtr("actType2"),
							},
							TaskList:                      taskList,
							ScheduleToCloseTimeoutSeconds: common.Int32Ptr(1000),
							ScheduleToStartTimeoutSeconds: common.Int32Ptr(2000),
							StartToCloseTimeoutSeconds:    common.Int32Ptr(3000),
							HeartbeatTimeoutSeconds:       common.Int32Ptr(4000),
							DecisionTaskCompletedEventId:  common.Int64Ptr(16),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(23),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskStarted),
						ActivityTaskStartedEventAttributes: &workflow.ActivityTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(21),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(24),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskStarted),
						ActivityTaskStartedEventAttributes: &workflow.ActivityTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(17),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(25),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskStarted),
						ActivityTaskStartedEventAttributes: &workflow.ActivityTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(22),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(26),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskCompleted),
						ActivityTaskCompletedEventAttributes: &workflow.ActivityTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(21),
							StartedEventId:   common.Int64Ptr(23),
						},
					},
					{
						EventId:   common.Int64Ptr(27),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskScheduled),
						DecisionTaskScheduledEventAttributes: &workflow.DecisionTaskScheduledEventAttributes{
							TaskList:                   taskList,
							StartToCloseTimeoutSeconds: common.Int32Ptr(100),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(28),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskStarted),
						DecisionTaskStartedEventAttributes: &workflow.DecisionTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(27),
						},
					},
				},
			},
			/////////////// reset point/////////////
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(29),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskCompleted),
						DecisionTaskCompletedEventAttributes: &workflow.DecisionTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(27),
							StartedEventId:   common.Int64Ptr(28),
						},
					},
					{
						EventId:   common.Int64Ptr(30),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeTimerStarted),
						TimerStartedEventAttributes: &workflow.TimerStartedEventAttributes{
							TimerId:                      common.StringPtr(timerAfterReset),
							StartToFireTimeoutSeconds:    common.Int64Ptr(4),
							DecisionTaskCompletedEventId: common.Int64Ptr(29),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(31),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskStarted),
						ActivityTaskStartedEventAttributes: &workflow.ActivityTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(18),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(32),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeWorkflowExecutionSignaled),
						WorkflowExecutionSignaledEventAttributes: &workflow.WorkflowExecutionSignaledEventAttributes{
							SignalName: common.StringPtr(signalName1),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(33),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeWorkflowExecutionSignaled),
						WorkflowExecutionSignaledEventAttributes: &workflow.WorkflowExecutionSignaledEventAttributes{
							SignalName: common.StringPtr(signalName2),
						},
					},
				},
			},
		},
	}

	eid := int64(0)
	for _, be := range readHistoryResp.History {
		for _, e := range be.Events {
			eid++
			if e.GetEventId() != eid {
				s.Fail(fmt.Sprintf("inconintous eventID: %v, %v", eid, e.GetEventId()))
			}
			e.Timestamp = common.Int64Ptr(1000)
		}
	}

	newBranchToken := []byte("newBranch")
	forkResp := &p.ForkHistoryBranchResponse{
		NewBranchToken: newBranchToken,
	}

	completeReq := &p.CompleteForkBranchRequest{
		BranchToken: newBranchToken,
		Success:     true,
	}
	completeReqErr := &p.CompleteForkBranchRequest{
		BranchToken: newBranchToken,
		Success:     false,
	}

	appendV1Resp := &p.AppendHistoryEventsResponse{
		Size: 100,
	}
	appendV2Resp := &p.AppendHistoryNodesResponse{
		Size: 200,
	}

	s.mockExecutionMgr.On("GetWorkflowExecution", forkGwmsRequest).Return(forkGwmsResponse, nil).Once()
	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(gcurResponse, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", currGwmsRequest).Return(currGwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("ReadHistoryBranchByBatch", readHistoryReq).Return(readHistoryResp, nil).Once()
	s.mockHistoryV2Mgr.On("ForkHistoryBranch", mock.Anything).Return(forkResp, nil).Once()
	s.mockHistoryV2Mgr.On("CompleteForkBranch", completeReq).Return(nil).Once()
	s.mockHistoryV2Mgr.On("CompleteForkBranch", completeReqErr).Return(nil).Maybe()
	s.mockHistoryMgr.On("AppendHistoryEvents", mock.Anything).Return(appendV1Resp, nil).Once()
	s.mockHistoryV2Mgr.On("AppendHistoryNodes", mock.Anything).Return(appendV2Resp, nil).Once()
	s.mockClusterMetadata.On("ClusterNameForFailoverVersion", mock.Anything).Return("test")
	s.mockExecutionMgr.On("ResetWorkflowExecution", mock.Anything).Return(nil).Once()
	response, err := s.historyEngine.ResetWorkflowExecution(context.Background(), request)
	s.Nil(err)
	s.NotNil(response.RunId)

	// verify historyEvent: 5 events to append
	// 1. decisionFailed :29
	// 2. activityFailed :30
	// 3. signal 1 :31
	// 4. signal 2 :32
	// 5. decisionTaskScheduled :33
	calls := s.mockHistoryV2Mgr.Calls
	s.Equal(4, len(calls))
	appendCall := calls[2]
	s.Equal("AppendHistoryNodes", appendCall.Method)
	appendReq, ok := appendCall.Arguments[0].(*p.AppendHistoryNodesRequest)
	s.Equal(true, ok)
	s.Equal(newBranchToken, appendReq.BranchToken)
	s.Equal(false, appendReq.IsNewBranch)
	s.Equal(6, len(appendReq.Events))
	s.Equal(workflow.EventTypeDecisionTaskFailed, appendReq.Events[0].GetEventType())
	s.Equal(workflow.EventTypeActivityTaskFailed, appendReq.Events[1].GetEventType())
	s.Equal(workflow.EventTypeActivityTaskFailed, appendReq.Events[2].GetEventType())
	s.Equal(workflow.EventTypeWorkflowExecutionSignaled, appendReq.Events[3].GetEventType())
	s.Equal(workflow.EventTypeWorkflowExecutionSignaled, appendReq.Events[4].GetEventType())
	s.Equal(workflow.EventTypeDecisionTaskScheduled, appendReq.Events[5].GetEventType())

	s.Equal(int64(29), appendReq.Events[0].GetEventId())
	s.Equal(int64(30), appendReq.Events[1].GetEventId())
	s.Equal(int64(31), appendReq.Events[2].GetEventId())
	s.Equal(int64(32), appendReq.Events[3].GetEventId())
	s.Equal(int64(33), appendReq.Events[4].GetEventId())
	s.Equal(int64(34), appendReq.Events[5].GetEventId())

	// verify executionManager request
	calls = s.mockExecutionMgr.Calls
	s.Equal(4, len(calls))
	resetCall := calls[3]
	s.Equal("ResetWorkflowExecution", resetCall.Method)
	resetReq, ok := resetCall.Arguments[0].(*p.ResetWorkflowExecutionRequest)
	s.Equal(true, ok)
	s.Equal(currRunID, resetReq.PrevRunID)
	s.Equal(true, resetReq.UpdateCurr)
	compareCurrExeInfo.State = p.WorkflowStateCompleted
	compareCurrExeInfo.CloseStatus = p.WorkflowCloseStatusTerminated
	compareCurrExeInfo.NextEventID = 2
	compareCurrExeInfo.HistorySize = 100
	s.Equal(compareCurrExeInfo, resetReq.CurrExecutionInfo)
	s.Equal(1, len(resetReq.CurrTransferTasks))
	s.Equal(1, len(resetReq.CurrTimerTasks))
	s.Equal(p.TransferTaskTypeCloseExecution, resetReq.CurrTransferTasks[0].GetType())
	s.Equal(p.TaskTypeDeleteHistoryEvent, resetReq.CurrTimerTasks[0].GetType())

	s.Equal("wfType", resetReq.InsertExecutionInfo.WorkflowTypeName)
	s.True(len(resetReq.InsertExecutionInfo.RunID) > 0)
	s.Equal([]byte(newBranchToken), resetReq.InsertExecutionInfo.BranchToken)
	// 35 = resetEventID(29) + 6 in a batch
	s.Equal(int64(34), resetReq.InsertExecutionInfo.DecisionScheduleID)
	s.Equal(int64(35), resetReq.InsertExecutionInfo.NextEventID)

	// one activity task and one decision task
	s.Equal(2, len(resetReq.InsertTransferTasks))
	s.Equal(p.TransferTaskTypeActivityTask, resetReq.InsertTransferTasks[0].GetType())
	s.Equal(p.TransferTaskTypeDecisionTask, resetReq.InsertTransferTasks[1].GetType())

	// WF timeout task, user timer, activity timeout timer, activity retry timer
	s.Equal(3, len(resetReq.InsertTimerTasks))
	s.Equal(p.TaskTypeWorkflowTimeout, resetReq.InsertTimerTasks[0].GetType())
	s.Equal(p.TaskTypeUserTimer, resetReq.InsertTimerTasks[1].GetType())
	s.Equal(p.TaskTypeActivityTimeout, resetReq.InsertTimerTasks[2].GetType())

	s.Equal(2, len(resetReq.InsertTimerInfos))
	s.assertTimerIDs([]string{timerUnfiredID1, timerUnfiredID2}, resetReq.InsertTimerInfos)

	s.Equal(1, len(resetReq.InsertActivityInfos))
	s.assertActivityIDs([]string{actIDNotStarted}, resetReq.InsertActivityInfos)

	s.Nil(resetReq.InsertReplicationTasks)
	s.Nil(resetReq.InsertReplicationState)
	s.Equal(0, len(resetReq.InsertRequestCancelInfos))

	// not supported feature
	s.Nil(resetReq.InsertChildExecutionInfos)
	s.Nil(resetReq.InsertSignalInfos)
	s.Nil(resetReq.InsertSignalRequestedIDs)
}

func (s *resetorSuite) assertTimerIDs(ids []string, timers []*p.TimerInfo) {
	m := map[string]bool{}
	for _, s := range ids {
		m[s] = true
	}

	for _, t := range timers {
		delete(m, t.TimerID)
	}

	s.Equal(0, len(m))
}

func (s *resetorSuite) assertActivityIDs(ids []string, timers []*p.ActivityInfo) {
	m := map[string]bool{}
	for _, s := range ids {
		m[s] = true
	}

	for _, t := range timers {
		delete(m, t.ActivityID)
	}

	s.Equal(0, len(m))
}

func (s *resetorSuite) TestResetWorkflowExecution_NoReplication_WithRequestCancel() {
	testDomainEntry := cache.NewDomainCacheEntryForTest(&p.DomainInfo{ID: validDomainID}, &p.DomainConfig{Retention: 1})
	s.mockDomainCache.On("GetDomainByID", mock.Anything).Return(testDomainEntry, nil)
	s.mockDomainCache.On("GetDomain", mock.Anything).Return(testDomainEntry, nil)

	request := &h.ResetWorkflowExecutionRequest{}
	_, err := s.historyEngine.ResetWorkflowExecution(context.Background(), request)
	s.EqualError(err, "BadRequestError{Message: Missing domain UUID.}")

	domainID := validDomainID
	request.DomainUUID = &domainID
	request.ResetRequest = &workflow.ResetWorkflowExecutionRequest{}
	_, err = s.historyEngine.ResetWorkflowExecution(context.Background(), request)
	s.EqualError(err, "BadRequestError{Message: Require workflowId and runId.}")

	wid := "wId"
	wfType := "wfType"
	taskListName := "taskList"
	forkRunID := uuid.New().String()
	currRunID := uuid.New().String()
	we := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(wid),
		RunId:      common.StringPtr(forkRunID),
	}
	request.ResetRequest = &workflow.ResetWorkflowExecutionRequest{
		Domain:                common.StringPtr("testDomainName"),
		WorkflowExecution:     &we,
		Reason:                common.StringPtr("test reset"),
		DecisionFinishEventId: common.Int64Ptr(30),
		RequestId:             common.StringPtr(uuid.New().String()),
	}

	forkGwmsRequest := &p.GetWorkflowExecutionRequest{
		DomainID: domainID,
		Execution: workflow.WorkflowExecution{
			WorkflowId: common.StringPtr(wid),
			RunId:      common.StringPtr(forkRunID),
		},
	}

	timerFiredID := "timerID0"
	timerUnfiredID1 := "timerID1"
	timerUnfiredID2 := "timerID2"
	timerAfterReset := "timerID3"
	actIDCompleted1 := "actID0"
	actIDCompleted2 := "actID1"
	actIDStartedRetry := "actID2"
	actIDNotStarted := "actID3"
	actIDStartedNoRetry := "actID4"
	signalName1 := "sig1"
	signalName2 := "sig2"
	cancelWE := &workflow.WorkflowExecution{
		WorkflowId: common.StringPtr("cancel-wfid"),
		RunId:      common.StringPtr(uuid.New().String()),
	}
	forkBranchToken := []byte("forkBranchToken")
	forkExeInfo := &p.WorkflowExecutionInfo{
		DomainID:          domainID,
		WorkflowID:        wid,
		WorkflowTypeName:  wfType,
		TaskList:          taskListName,
		RunID:             forkRunID,
		EventStoreVersion: p.EventStoreVersionV2,
		BranchToken:       forkBranchToken,
		NextEventID:       35,
	}
	forkGwmsResponse := &p.GetWorkflowExecutionResponse{State: &p.WorkflowMutableState{
		ExecutionInfo: forkExeInfo,
	}}

	currGwmsRequest := &p.GetWorkflowExecutionRequest{
		DomainID: domainID,
		Execution: workflow.WorkflowExecution{
			WorkflowId: common.StringPtr(wid),
			RunId:      common.StringPtr(currRunID),
		},
	}
	currExeInfo := &p.WorkflowExecutionInfo{
		DomainID:         domainID,
		WorkflowID:       wid,
		WorkflowTypeName: wfType,
		TaskList:         taskListName,
		RunID:            currRunID,
		NextEventID:      common.FirstEventID,
	}
	currGwmsResponse := &p.GetWorkflowExecutionResponse{State: &p.WorkflowMutableState{
		ExecutionInfo: currExeInfo,
	}}

	gcurResponse := &p.GetCurrentExecutionResponse{
		RunID: currRunID,
	}

	readHistoryReq := &p.ReadHistoryBranchRequest{
		BranchToken:   forkBranchToken,
		MinEventID:    common.FirstEventID,
		MaxEventID:    int64(35),
		PageSize:      defaultHistoryPageSize,
		NextPageToken: nil,
	}

	taskList := &workflow.TaskList{
		Name: common.StringPtr(taskListName),
	}
	readHistoryResp := &p.ReadHistoryBranchByBatchResponse{
		NextPageToken:    nil,
		Size:             1000,
		LastFirstEventID: int64(31),
		History: []*workflow.History{
			&workflow.History{
				Events: []*workflow.HistoryEvent{
					&workflow.HistoryEvent{
						EventId:   common.Int64Ptr(1),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeWorkflowExecutionStarted),
						WorkflowExecutionStartedEventAttributes: &workflow.WorkflowExecutionStartedEventAttributes{
							WorkflowType: &workflow.WorkflowType{
								Name: common.StringPtr(wfType),
							},
							TaskList:                            taskList,
							Input:                               []byte("testInput"),
							ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(100),
							TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(200),
						},
					},
					{
						EventId:   common.Int64Ptr(2),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskScheduled),
						DecisionTaskScheduledEventAttributes: &workflow.DecisionTaskScheduledEventAttributes{
							TaskList:                   taskList,
							StartToCloseTimeoutSeconds: common.Int32Ptr(100),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(3),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskStarted),
						DecisionTaskStartedEventAttributes: &workflow.DecisionTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(2),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(4),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskCompleted),
						DecisionTaskCompletedEventAttributes: &workflow.DecisionTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(2),
							StartedEventId:   common.Int64Ptr(3),
						},
					},
					{
						EventId:   common.Int64Ptr(5),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeMarkerRecorded),
						MarkerRecordedEventAttributes: &workflow.MarkerRecordedEventAttributes{
							MarkerName:                   common.StringPtr("Version"),
							Details:                      []byte("details"),
							DecisionTaskCompletedEventId: common.Int64Ptr(4),
						},
					},
					{
						EventId:   common.Int64Ptr(6),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskScheduled),
						ActivityTaskScheduledEventAttributes: &workflow.ActivityTaskScheduledEventAttributes{
							ActivityId: common.StringPtr(actIDCompleted1),
							ActivityType: &workflow.ActivityType{
								Name: common.StringPtr("actType0"),
							},
							TaskList:                      taskList,
							ScheduleToCloseTimeoutSeconds: common.Int32Ptr(1000),
							ScheduleToStartTimeoutSeconds: common.Int32Ptr(2000),
							StartToCloseTimeoutSeconds:    common.Int32Ptr(3000),
							HeartbeatTimeoutSeconds:       common.Int32Ptr(4000),
							DecisionTaskCompletedEventId:  common.Int64Ptr(4),
						},
					},
					{
						EventId:   common.Int64Ptr(7),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeTimerStarted),
						TimerStartedEventAttributes: &workflow.TimerStartedEventAttributes{
							TimerId:                      common.StringPtr(timerFiredID),
							StartToFireTimeoutSeconds:    common.Int64Ptr(2),
							DecisionTaskCompletedEventId: common.Int64Ptr(4),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(8),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskStarted),
						ActivityTaskStartedEventAttributes: &workflow.ActivityTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(6),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(9),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskCompleted),
						ActivityTaskCompletedEventAttributes: &workflow.ActivityTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(6),
							StartedEventId:   common.Int64Ptr(8),
						},
					},
					{
						EventId:   common.Int64Ptr(10),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskScheduled),
						DecisionTaskScheduledEventAttributes: &workflow.DecisionTaskScheduledEventAttributes{
							TaskList:                   taskList,
							StartToCloseTimeoutSeconds: common.Int32Ptr(100),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(11),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskStarted),
						DecisionTaskStartedEventAttributes: &workflow.DecisionTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(10),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(12),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskCompleted),
						DecisionTaskCompletedEventAttributes: &workflow.DecisionTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(10),
							StartedEventId:   common.Int64Ptr(11),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(13),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeTimerFired),
						TimerFiredEventAttributes: &workflow.TimerFiredEventAttributes{
							TimerId: common.StringPtr(timerFiredID),
						},
					},
					{
						EventId:   common.Int64Ptr(14),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskScheduled),
						DecisionTaskScheduledEventAttributes: &workflow.DecisionTaskScheduledEventAttributes{
							TaskList:                   taskList,
							StartToCloseTimeoutSeconds: common.Int32Ptr(100),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(15),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskStarted),
						DecisionTaskStartedEventAttributes: &workflow.DecisionTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(14),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(16),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskCompleted),
						DecisionTaskCompletedEventAttributes: &workflow.DecisionTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(14),
							StartedEventId:   common.Int64Ptr(15),
						},
					},
					{
						EventId:   common.Int64Ptr(17),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskScheduled),
						ActivityTaskScheduledEventAttributes: &workflow.ActivityTaskScheduledEventAttributes{
							ActivityId: common.StringPtr(actIDStartedRetry),
							ActivityType: &workflow.ActivityType{
								Name: common.StringPtr("actType1"),
							},
							TaskList:                      taskList,
							ScheduleToCloseTimeoutSeconds: common.Int32Ptr(1000),
							ScheduleToStartTimeoutSeconds: common.Int32Ptr(2000),
							StartToCloseTimeoutSeconds:    common.Int32Ptr(3000),
							HeartbeatTimeoutSeconds:       common.Int32Ptr(4000),
							DecisionTaskCompletedEventId:  common.Int64Ptr(16),
							RetryPolicy: &workflow.RetryPolicy{
								InitialIntervalInSeconds:    common.Int32Ptr(1),
								BackoffCoefficient:          common.Float64Ptr(0.2),
								MaximumAttempts:             common.Int32Ptr(10),
								MaximumIntervalInSeconds:    common.Int32Ptr(1000),
								ExpirationIntervalInSeconds: common.Int32Ptr(math.MaxInt32),
							},
						},
					},
					{
						EventId:   common.Int64Ptr(18),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskScheduled),
						ActivityTaskScheduledEventAttributes: &workflow.ActivityTaskScheduledEventAttributes{
							ActivityId: common.StringPtr(actIDNotStarted),
							ActivityType: &workflow.ActivityType{
								Name: common.StringPtr("actType2"),
							},
							TaskList:                      taskList,
							ScheduleToCloseTimeoutSeconds: common.Int32Ptr(1000),
							ScheduleToStartTimeoutSeconds: common.Int32Ptr(2000),
							StartToCloseTimeoutSeconds:    common.Int32Ptr(3000),
							HeartbeatTimeoutSeconds:       common.Int32Ptr(4000),
							DecisionTaskCompletedEventId:  common.Int64Ptr(16),
						},
					},
					{
						EventId:   common.Int64Ptr(19),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeTimerStarted),
						TimerStartedEventAttributes: &workflow.TimerStartedEventAttributes{
							TimerId:                      common.StringPtr(timerUnfiredID1),
							StartToFireTimeoutSeconds:    common.Int64Ptr(4),
							DecisionTaskCompletedEventId: common.Int64Ptr(16),
						},
					},
					{
						EventId:   common.Int64Ptr(20),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeTimerStarted),
						TimerStartedEventAttributes: &workflow.TimerStartedEventAttributes{
							TimerId:                      common.StringPtr(timerUnfiredID2),
							StartToFireTimeoutSeconds:    common.Int64Ptr(8),
							DecisionTaskCompletedEventId: common.Int64Ptr(16),
						},
					},
					{
						EventId:   common.Int64Ptr(21),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskScheduled),
						ActivityTaskScheduledEventAttributes: &workflow.ActivityTaskScheduledEventAttributes{
							ActivityId: common.StringPtr(actIDCompleted2),
							ActivityType: &workflow.ActivityType{
								Name: common.StringPtr("actType2"),
							},
							TaskList:                      taskList,
							ScheduleToCloseTimeoutSeconds: common.Int32Ptr(1000),
							ScheduleToStartTimeoutSeconds: common.Int32Ptr(2000),
							StartToCloseTimeoutSeconds:    common.Int32Ptr(3000),
							HeartbeatTimeoutSeconds:       common.Int32Ptr(4000),
							DecisionTaskCompletedEventId:  common.Int64Ptr(16),
						},
					},
					{
						EventId:   common.Int64Ptr(22),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskScheduled),
						ActivityTaskScheduledEventAttributes: &workflow.ActivityTaskScheduledEventAttributes{
							ActivityId: common.StringPtr(actIDStartedNoRetry),
							ActivityType: &workflow.ActivityType{
								Name: common.StringPtr("actType2"),
							},
							TaskList:                      taskList,
							ScheduleToCloseTimeoutSeconds: common.Int32Ptr(1000),
							ScheduleToStartTimeoutSeconds: common.Int32Ptr(2000),
							StartToCloseTimeoutSeconds:    common.Int32Ptr(3000),
							HeartbeatTimeoutSeconds:       common.Int32Ptr(4000),
							DecisionTaskCompletedEventId:  common.Int64Ptr(16),
						},
					},
					{
						EventId:   common.Int64Ptr(23),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeRequestCancelExternalWorkflowExecutionInitiated),
						RequestCancelExternalWorkflowExecutionInitiatedEventAttributes: &workflow.RequestCancelExternalWorkflowExecutionInitiatedEventAttributes{
							Domain:                       common.StringPtr("any-domain-name"),
							WorkflowExecution:            cancelWE,
							DecisionTaskCompletedEventId: common.Int64Ptr(16),
							ChildWorkflowOnly:            common.BoolPtr(true),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(24),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskStarted),
						ActivityTaskStartedEventAttributes: &workflow.ActivityTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(21),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(25),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskStarted),
						ActivityTaskStartedEventAttributes: &workflow.ActivityTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(17),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(26),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskStarted),
						ActivityTaskStartedEventAttributes: &workflow.ActivityTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(22),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(27),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskCompleted),
						ActivityTaskCompletedEventAttributes: &workflow.ActivityTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(21),
							StartedEventId:   common.Int64Ptr(24),
						},
					},
					{
						EventId:   common.Int64Ptr(28),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskScheduled),
						DecisionTaskScheduledEventAttributes: &workflow.DecisionTaskScheduledEventAttributes{
							TaskList:                   taskList,
							StartToCloseTimeoutSeconds: common.Int32Ptr(100),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(29),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskStarted),
						DecisionTaskStartedEventAttributes: &workflow.DecisionTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(28),
						},
					},
				},
			},
			/////////////// reset point/////////////
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(30),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeDecisionTaskCompleted),
						DecisionTaskCompletedEventAttributes: &workflow.DecisionTaskCompletedEventAttributes{
							ScheduledEventId: common.Int64Ptr(28),
							StartedEventId:   common.Int64Ptr(29),
						},
					},
					{
						EventId:   common.Int64Ptr(31),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeTimerStarted),
						TimerStartedEventAttributes: &workflow.TimerStartedEventAttributes{
							TimerId:                      common.StringPtr(timerAfterReset),
							StartToFireTimeoutSeconds:    common.Int64Ptr(4),
							DecisionTaskCompletedEventId: common.Int64Ptr(30),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(32),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeActivityTaskStarted),
						ActivityTaskStartedEventAttributes: &workflow.ActivityTaskStartedEventAttributes{
							ScheduledEventId: common.Int64Ptr(18),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(33),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeWorkflowExecutionSignaled),
						WorkflowExecutionSignaledEventAttributes: &workflow.WorkflowExecutionSignaledEventAttributes{
							SignalName: common.StringPtr(signalName1),
						},
					},
				},
			},
			{
				Events: []*workflow.HistoryEvent{
					{
						EventId:   common.Int64Ptr(34),
						Version:   common.Int64Ptr(common.EmptyVersion),
						EventType: common.EventTypePtr(workflow.EventTypeWorkflowExecutionSignaled),
						WorkflowExecutionSignaledEventAttributes: &workflow.WorkflowExecutionSignaledEventAttributes{
							SignalName: common.StringPtr(signalName2),
						},
					},
				},
			},
		},
	}

	eid := int64(0)
	for _, be := range readHistoryResp.History {
		for _, e := range be.Events {
			eid++
			if e.GetEventId() != eid {
				s.Fail(fmt.Sprintf("inconintous eventID: %v, %v", eid, e.GetEventId()))
			}
			e.Timestamp = common.Int64Ptr(1000)
		}
	}

	s.mockExecutionMgr.On("GetWorkflowExecution", forkGwmsRequest).Return(forkGwmsResponse, nil).Once()
	s.mockExecutionMgr.On("GetCurrentExecution", mock.Anything).Return(gcurResponse, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", currGwmsRequest).Return(currGwmsResponse, nil).Once()
	s.mockHistoryV2Mgr.On("ReadHistoryBranchByBatch", readHistoryReq).Return(readHistoryResp, nil).Once()
	s.mockHistoryV2Mgr.On("CompleteForkBranch", mock.Anything).Return(nil).Maybe()
	_, err = s.historyEngine.ResetWorkflowExecution(context.Background(), request)
	s.EqualError(err, "BadRequestError{Message: it is not allowed resetting to a point that workflow has pending request cancel }")
}
