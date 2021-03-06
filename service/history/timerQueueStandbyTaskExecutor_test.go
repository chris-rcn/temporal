// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
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
	"testing"
	"time"

	"github.com/gogo/protobuf/types"
	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
	commonpb "go.temporal.io/temporal-proto/common"
	eventpb "go.temporal.io/temporal-proto/event"
	executionpb "go.temporal.io/temporal-proto/execution"
	tasklistpb "go.temporal.io/temporal-proto/tasklist"
	"go.temporal.io/temporal-proto/workflowservice"

	"github.com/temporalio/temporal/.gen/proto/historyservice"
	"github.com/temporalio/temporal/.gen/proto/persistenceblobs"
	"github.com/temporalio/temporal/common"
	"github.com/temporalio/temporal/common/cache"
	"github.com/temporalio/temporal/common/clock"
	"github.com/temporalio/temporal/common/cluster"
	"github.com/temporalio/temporal/common/log"
	"github.com/temporalio/temporal/common/metrics"
	"github.com/temporalio/temporal/common/mocks"
	"github.com/temporalio/temporal/common/persistence"
	"github.com/temporalio/temporal/common/primitives"
	"github.com/temporalio/temporal/common/xdc"
)

type (
	timerQueueStandbyTaskExecutorSuite struct {
		suite.Suite
		*require.Assertions

		controller               *gomock.Controller
		mockShard                *shardContextTest
		mockTxProcessor          *MocktransferQueueProcessor
		mockReplicationProcessor *MockReplicatorQueueProcessor
		mockTimerProcessor       *MocktimerQueueProcessor
		mockNamespaceCache       *cache.MockNamespaceCache
		mockClusterMetadata      *cluster.MockMetadata
		mockNDCHistoryResender   *xdc.MockNDCHistoryResender

		mockExecutionMgr        *mocks.ExecutionManager
		mockHistoryRereplicator *xdc.MockHistoryRereplicator

		logger               log.Logger
		namespaceID          string
		namespaceEntry       *cache.NamespaceCacheEntry
		version              int64
		clusterName          string
		now                  time.Time
		timeSource           *clock.EventTimeSource
		fetchHistoryDuration time.Duration
		discardDuration      time.Duration

		timerQueueStandbyTaskExecutor *timerQueueStandbyTaskExecutor
	}
)

func TestTimerQueueStandbyTaskExecutorSuite(t *testing.T) {
	s := new(timerQueueStandbyTaskExecutorSuite)
	suite.Run(t, s)
}

func (s *timerQueueStandbyTaskExecutorSuite) SetupSuite() {

}

func (s *timerQueueStandbyTaskExecutorSuite) SetupTest() {
	s.Assertions = require.New(s.T())

	config := NewDynamicConfigForTest()
	s.namespaceID = testNamespaceID
	s.namespaceEntry = testGlobalNamespaceEntry
	s.version = s.namespaceEntry.GetFailoverVersion()
	s.clusterName = cluster.TestAlternativeClusterName
	s.now = time.Now()
	s.timeSource = clock.NewEventTimeSource().Update(s.now)
	s.fetchHistoryDuration = config.StandbyTaskMissingEventsResendDelay() +
		(config.StandbyTaskMissingEventsDiscardDelay()-config.StandbyTaskMissingEventsResendDelay())/2
	s.discardDuration = config.StandbyTaskMissingEventsDiscardDelay() * 2

	s.controller = gomock.NewController(s.T())
	s.mockTxProcessor = NewMocktransferQueueProcessor(s.controller)
	s.mockReplicationProcessor = NewMockReplicatorQueueProcessor(s.controller)
	s.mockTimerProcessor = NewMocktimerQueueProcessor(s.controller)
	s.mockNDCHistoryResender = xdc.NewMockNDCHistoryResender(s.controller)
	s.mockTxProcessor.EXPECT().NotifyNewTask(gomock.Any(), gomock.Any()).AnyTimes()
	s.mockReplicationProcessor.EXPECT().notifyNewTask().AnyTimes()
	s.mockTimerProcessor.EXPECT().NotifyNewTimers(gomock.Any(), gomock.Any()).AnyTimes()

	s.mockHistoryRereplicator = &xdc.MockHistoryRereplicator{}

	s.mockShard = newTestShardContext(
		s.controller,
		&persistence.ShardInfoWithFailover{
			ShardInfo: &persistenceblobs.ShardInfo{
				RangeId:          1,
				TransferAckLevel: 0,
			}},
		config,
	)
	s.mockShard.eventsCache = newEventsCache(s.mockShard)
	s.mockShard.resource.TimeSource = s.timeSource

	// ack manager will use the namespace information
	s.mockNamespaceCache = s.mockShard.resource.NamespaceCache
	s.mockExecutionMgr = s.mockShard.resource.ExecutionMgr
	s.mockClusterMetadata = s.mockShard.resource.ClusterMetadata
	s.mockNamespaceCache.EXPECT().GetNamespaceByID(gomock.Any()).Return(testGlobalNamespaceEntry, nil).AnyTimes()
	s.mockClusterMetadata.EXPECT().GetCurrentClusterName().Return(cluster.TestCurrentClusterName).AnyTimes()
	s.mockClusterMetadata.EXPECT().GetAllClusterInfo().Return(cluster.TestAllClusterInfo).AnyTimes()
	s.mockClusterMetadata.EXPECT().IsGlobalNamespaceEnabled().Return(true).AnyTimes()
	s.mockClusterMetadata.EXPECT().ClusterNameForFailoverVersion(s.version).Return(s.clusterName).AnyTimes()

	s.logger = s.mockShard.GetLogger()

	historyCache := newHistoryCache(s.mockShard)
	h := &historyEngineImpl{
		currentClusterName:   s.mockShard.GetService().GetClusterMetadata().GetCurrentClusterName(),
		shard:                s.mockShard,
		clusterMetadata:      s.mockClusterMetadata,
		executionManager:     s.mockExecutionMgr,
		historyCache:         historyCache,
		logger:               s.logger,
		tokenSerializer:      common.NewProtoTaskTokenSerializer(),
		metricsClient:        s.mockShard.GetMetricsClient(),
		historyEventNotifier: newHistoryEventNotifier(s.timeSource, metrics.NewClient(tally.NoopScope, metrics.History), func(string) int { return 0 }),
		txProcessor:          s.mockTxProcessor,
		replicatorProcessor:  s.mockReplicationProcessor,
		timerProcessor:       s.mockTimerProcessor,
	}
	s.mockShard.SetEngine(h)

	s.timerQueueStandbyTaskExecutor = newTimerQueueStandbyTaskExecutor(
		s.mockShard,
		h,
		s.mockHistoryRereplicator,
		s.mockNDCHistoryResender,
		s.logger,
		s.mockShard.GetMetricsClient(),
		s.clusterName,
		config,
		// newTaskAllocator(s.mockShard),
	).(*timerQueueStandbyTaskExecutor)
}

func (s *timerQueueStandbyTaskExecutorSuite) TearDownTest() {
	s.controller.Finish()
	s.mockShard.Finish(s.T())
	s.mockHistoryRereplicator.AssertExpectations(s.T())
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessUserTimerTimeout_Pending() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(
		s.mockShard,
		s.mockShard.GetEventsCache(),
		s.logger,
		s.version,
		execution.GetRunId(),
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)
	event := addDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerID := "timer"
	timerTimeout := 2 * time.Second
	event, _ = addTimerStartedEvent(mutableState, event.GetEventId(), timerID, int64(timerTimeout.Seconds()))
	nextEventID := event.GetEventId() + 1

	timerSequence := newTimerSequence(s.timeSource, mutableState)
	mutableState.insertTimerTasks = nil
	modified, err := timerSequence.createNextUserTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.insertTimerTasks[0]
	protoTaskTime, err := types.TimestampProto(task.(*persistence.UserTimerTask).GetVisibilityTimestamp())
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeUserTimer,
		TimeoutType:         int32(eventpb.TimeoutType_StartToClose),
		VisibilityTimestamp: protoTaskTime,
		EventId:             event.EventId,
	}

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventId(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil)

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskRetry, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.fetchHistoryDuration))
	s.mockHistoryRereplicator.On("SendMultiWorkflowHistory",
		primitives.UUIDString(timerTask.GetNamespaceId()), timerTask.GetWorkflowId(),
		primitives.UUIDString(timerTask.GetRunId()), nextEventID,
		primitives.UUIDString(timerTask.GetRunId()), common.EndEventID,
	).Return(nil).Once()
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskRetry, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.discardDuration))
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskDiscarded, err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessUserTimerTimeout_Success() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)
	event := addDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerID := "timer"
	timerTimeout := 2 * time.Second
	event, _ = addTimerStartedEvent(mutableState, event.GetEventId(), timerID, int64(timerTimeout.Seconds()))

	timerSequence := newTimerSequence(s.timeSource, mutableState)
	mutableState.insertTimerTasks = nil
	modified, err := timerSequence.createNextUserTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.insertTimerTasks[0]
	protoTaskTime, err := types.TimestampProto(task.(*persistence.UserTimerTask).GetVisibilityTimestamp())
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeUserTimer,
		TimeoutType:         int32(eventpb.TimeoutType_StartToClose),
		VisibilityTimestamp: protoTaskTime,
		EventId:             event.EventId,
	}

	event = addTimerFiredEvent(mutableState, timerID)

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventId(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Nil(err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessUserTimerTimeout_Multiple() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)
	event := addDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerID1 := "timer-1"
	timerTimeout1 := 2 * time.Second
	event, _ = addTimerStartedEvent(mutableState, event.GetEventId(), timerID1, int64(timerTimeout1.Seconds()))

	timerID2 := "timer-2"
	timerTimeout2 := 50 * time.Second
	_, _ = addTimerStartedEvent(mutableState, event.GetEventId(), timerID2, int64(timerTimeout2.Seconds()))

	timerSequence := newTimerSequence(s.timeSource, mutableState)
	mutableState.insertTimerTasks = nil
	modified, err := timerSequence.createNextUserTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.insertTimerTasks[0]
	protoTaskTime, err := types.TimestampProto(task.(*persistence.UserTimerTask).GetVisibilityTimestamp())
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeUserTimer,
		TimeoutType:         int32(eventpb.TimeoutType_StartToClose),
		VisibilityTimestamp: protoTaskTime,
		EventId:             event.EventId,
	}

	event = addTimerFiredEvent(mutableState, timerID1)

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventId(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Nil(err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessActivityTimeout_Pending() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)
	event := addDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	tasklist := "tasklist"
	activityID := "activity"
	activityType := "activity type"
	timerTimeout := 2 * time.Second
	scheduledEvent, _ := addActivityTaskScheduledEvent(mutableState, event.GetEventId(), activityID, activityType, tasklist, []byte(nil),
		int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()))
	nextEventID := scheduledEvent.GetEventId() + 1

	timerSequence := newTimerSequence(s.timeSource, mutableState)
	mutableState.insertTimerTasks = nil
	modified, err := timerSequence.createNextActivityTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.insertTimerTasks[0]
	protoTaskTime, err := types.TimestampProto(task.(*persistence.ActivityTimeoutTask).GetVisibilityTimestamp())
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeActivityTimeout,
		TimeoutType:         int32(eventpb.TimeoutType_ScheduleToClose),
		VisibilityTimestamp: protoTaskTime,
		EventId:             event.EventId,
	}

	persistenceMutableState := s.createPersistenceMutableState(mutableState, scheduledEvent.GetEventId(), scheduledEvent.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskRetry, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.fetchHistoryDuration))
	s.mockHistoryRereplicator.On("SendMultiWorkflowHistory",
		primitives.UUIDString(timerTask.GetNamespaceId()), timerTask.GetWorkflowId(),
		primitives.UUIDString(timerTask.GetRunId()), nextEventID,
		primitives.UUIDString(timerTask.GetRunId()), common.EndEventID,
	).Return(nil).Once()
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskRetry, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.discardDuration))
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskDiscarded, err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessActivityTimeout_Success() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)
	event := addDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	identity := "identity"
	tasklist := "tasklist"
	activityID := "activity"
	activityType := "activity type"
	timerTimeout := 2 * time.Second
	scheduledEvent, _ := addActivityTaskScheduledEvent(mutableState, event.GetEventId(), activityID, activityType, tasklist, []byte(nil),
		int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()))
	startedEvent := addActivityTaskStartedEvent(mutableState, scheduledEvent.GetEventId(), identity)

	timerSequence := newTimerSequence(s.timeSource, mutableState)
	mutableState.insertTimerTasks = nil
	modified, err := timerSequence.createNextActivityTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.insertTimerTasks[0]
	protoTaskTime, err := types.TimestampProto(task.(*persistence.ActivityTimeoutTask).GetVisibilityTimestamp())
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeActivityTimeout,
		TimeoutType:         int32(eventpb.TimeoutType_ScheduleToClose),
		VisibilityTimestamp: protoTaskTime,
		EventId:             event.GetEventId(),
	}

	completeEvent := addActivityTaskCompletedEvent(mutableState, scheduledEvent.GetEventId(), startedEvent.GetEventId(), []byte(nil), identity)

	persistenceMutableState := s.createPersistenceMutableState(mutableState, completeEvent.GetEventId(), completeEvent.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Nil(err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessActivityTimeout_Heartbeat_Noop() {
	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)
	event := addDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	identity := "identity"
	tasklist := "tasklist"
	activityID := "activity"
	activityType := "activity type"
	timerTimeout := 2 * time.Second
	heartbeatTimerTimeout := time.Second
	scheduledEvent, _ := addActivityTaskScheduledEvent(mutableState, event.GetEventId(), activityID, activityType, tasklist, []byte(nil),
		int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(heartbeatTimerTimeout.Seconds()))
	startedEvent := addActivityTaskStartedEvent(mutableState, scheduledEvent.GetEventId(), identity)

	timerSequence := newTimerSequence(s.timeSource, mutableState)
	mutableState.insertTimerTasks = nil
	modified, err := timerSequence.createNextActivityTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.insertTimerTasks[0]
	s.Equal(int(timerTypeHeartbeat), task.(*persistence.ActivityTimeoutTask).TimeoutType)
	protoTaskTime, err := types.TimestampProto(task.(*persistence.ActivityTimeoutTask).GetVisibilityTimestamp().Add(-time.Second))
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeActivityTimeout,
		TimeoutType:         int32(eventpb.TimeoutType_Heartbeat),
		VisibilityTimestamp: protoTaskTime,
		EventId:             scheduledEvent.GetEventId(),
	}

	persistenceMutableState := s.createPersistenceMutableState(mutableState, startedEvent.GetEventId(), startedEvent.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Nil(err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessActivityTimeout_Multiple_CanUpdate() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)
	event := addDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	identity := "identity"
	tasklist := "tasklist"
	activityID1 := "activity 1"
	activityType1 := "activity type 1"
	timerTimeout1 := 2 * time.Second
	scheduledEvent1, _ := addActivityTaskScheduledEvent(mutableState, event.GetEventId(), activityID1, activityType1, tasklist, []byte(nil),
		int32(timerTimeout1.Seconds()), int32(timerTimeout1.Seconds()), int32(timerTimeout1.Seconds()), int32(timerTimeout1.Seconds()))
	startedEvent1 := addActivityTaskStartedEvent(mutableState, scheduledEvent1.GetEventId(), identity)

	activityID2 := "activity 2"
	activityType2 := "activity type 2"
	timerTimeout2 := 20 * time.Second
	scheduledEvent2, _ := addActivityTaskScheduledEvent(mutableState, event.GetEventId(), activityID2, activityType2, tasklist, []byte(nil),
		int32(timerTimeout2.Seconds()), int32(timerTimeout2.Seconds()), int32(timerTimeout2.Seconds()), int32(timerTimeout2.Seconds()))
	addActivityTaskStartedEvent(mutableState, scheduledEvent2.GetEventId(), identity)
	activityInfo2 := mutableState.pendingActivityInfoIDs[scheduledEvent2.GetEventId()]
	activityInfo2.TimerTaskStatus |= timerTaskStatusCreatedHeartbeat
	activityInfo2.LastHeartBeatUpdatedTime = time.Now()

	timerSequence := newTimerSequence(s.timeSource, mutableState)
	mutableState.insertTimerTasks = nil
	modified, err := timerSequence.createNextActivityTimer()
	s.NoError(err)
	s.True(modified)
	protoTime, err := types.TimestampProto(activityInfo2.LastHeartBeatUpdatedTime.Add(-5 * time.Second))
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeActivityTimeout,
		TimeoutType:         int32(eventpb.TimeoutType_Heartbeat),
		VisibilityTimestamp: protoTime,
		EventId:             scheduledEvent2.GetEventId(),
	}

	completeEvent1 := addActivityTaskCompletedEvent(mutableState, scheduledEvent1.GetEventId(), startedEvent1.GetEventId(), []byte(nil), identity)

	persistenceMutableState := s.createPersistenceMutableState(mutableState, completeEvent1.GetEventId(), completeEvent1.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.MatchedBy(func(input *persistence.UpdateWorkflowExecutionRequest) bool {
		s.Equal(1, len(input.UpdateWorkflowMutation.TimerTasks))
		s.Equal(1, len(input.UpdateWorkflowMutation.UpsertActivityInfos))
		mutableState.executionInfo.LastUpdatedTimestamp = input.UpdateWorkflowMutation.ExecutionInfo.LastUpdatedTimestamp
		input.RangeID = 0
		input.UpdateWorkflowMutation.ExecutionInfo.LastEventTaskID = 0
		mutableState.executionInfo.LastEventTaskID = 0
		mutableState.executionInfo.DecisionOriginalScheduledTimestamp = input.UpdateWorkflowMutation.ExecutionInfo.DecisionOriginalScheduledTimestamp
		s.Equal(&persistence.UpdateWorkflowExecutionRequest{
			UpdateWorkflowMutation: persistence.WorkflowMutation{
				ExecutionInfo:             mutableState.executionInfo,
				ExecutionStats:            &persistence.ExecutionStats{},
				ReplicationState:          mutableState.replicationState,
				TransferTasks:             nil,
				ReplicationTasks:          nil,
				TimerTasks:                input.UpdateWorkflowMutation.TimerTasks,
				Condition:                 mutableState.GetNextEventID(),
				UpsertActivityInfos:       input.UpdateWorkflowMutation.UpsertActivityInfos,
				DeleteActivityInfos:       []int64{},
				UpsertTimerInfos:          []*persistenceblobs.TimerInfo{},
				DeleteTimerInfos:          []string{},
				UpsertChildExecutionInfos: []*persistence.ChildExecutionInfo{},
				DeleteChildExecutionInfo:  nil,
				UpsertRequestCancelInfos:  []*persistenceblobs.RequestCancelInfo{},
				DeleteRequestCancelInfo:   nil,
				UpsertSignalInfos:         []*persistenceblobs.SignalInfo{},
				DeleteSignalInfo:          nil,
				UpsertSignalRequestedIDs:  []string{},
				DeleteSignalRequestedID:   "",
				NewBufferedEvents:         nil,
				ClearBufferedEvents:       false,
			},
			NewWorkflowSnapshot: nil,
			Encoding:            common.EncodingType(s.mockShard.GetConfig().EventEncodingType(s.namespaceID)),
		}, input)
		return true
	})).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Nil(err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessDecisionTimeout_Pending() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)
	startedEvent := addDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	nextEventID := startedEvent.GetEventId() + 1

	protoTime, err := types.TimestampProto(s.now)
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeDecisionTimeout,
		TimeoutType:         int32(eventpb.TimeoutType_StartToClose),
		VisibilityTimestamp: protoTime,
		EventId:             di.ScheduleID,
	}

	persistenceMutableState := s.createPersistenceMutableState(mutableState, startedEvent.GetEventId(), startedEvent.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskRetry, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.fetchHistoryDuration))
	s.mockHistoryRereplicator.On("SendMultiWorkflowHistory",
		primitives.UUIDString(timerTask.GetNamespaceId()), timerTask.GetWorkflowId(),
		primitives.UUIDString(timerTask.GetRunId()), nextEventID,
		primitives.UUIDString(timerTask.GetRunId()), common.EndEventID,
	).Return(nil).Once()
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskRetry, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.discardDuration))
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskDiscarded, err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessDecisionTimeout_ScheduleToStartTimer() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}

	decisionScheduleID := int64(16384)

	protoTaskTime, err := types.TimestampProto(s.now)
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeDecisionTimeout,
		TimeoutType:         int32(eventpb.TimeoutType_ScheduleToStart),
		VisibilityTimestamp: protoTaskTime,
		EventId:             decisionScheduleID,
	}

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(nil, err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessDecisionTimeout_Success() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)
	event := addDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	protoTime, err := types.TimestampProto(s.now)
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeDecisionTimeout,
		TimeoutType:         int32(eventpb.TimeoutType_StartToClose),
		VisibilityTimestamp: protoTime,
		EventId:             di.ScheduleID,
	}

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventId(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Nil(err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessWorkflowBackoffTimer_Pending() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	event, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)
	nextEventID := event.GetEventId() + 1

	protoTaskTime, err := types.TimestampProto(s.now)
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeWorkflowBackoffTimer,
		VisibilityTimestamp: protoTaskTime,
	}

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventId(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskRetry, err)

	s.mockShard.SetCurrentTime(s.clusterName, time.Now().Add(s.fetchHistoryDuration))
	s.mockHistoryRereplicator.On("SendMultiWorkflowHistory",
		primitives.UUIDString(timerTask.GetNamespaceId()), timerTask.GetWorkflowId(),
		primitives.UUIDString(timerTask.GetRunId()), nextEventID,
		primitives.UUIDString(timerTask.GetRunId()), common.EndEventID,
	).Return(nil).Once()
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskRetry, err)

	s.mockShard.SetCurrentTime(s.clusterName, time.Now().Add(s.discardDuration))
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskDiscarded, err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessWorkflowBackoffTimer_Success() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)

	protoTaskTime, err := types.TimestampProto(s.now)
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeWorkflowBackoffTimer,
		VisibilityTimestamp: protoTaskTime,
	}

	persistenceMutableState := s.createPersistenceMutableState(mutableState, di.ScheduleID, di.Version)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Nil(err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessWorkflowTimeout_Pending() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)
	startEvent := addDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = startEvent.GetEventId()
	completionEvent := addDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")
	nextEventID := completionEvent.GetEventId() + 1

	protoTaskTime, err := types.TimestampProto(s.now)
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeWorkflowTimeout,
		TimeoutType:         int32(eventpb.TimeoutType_StartToClose),
		VisibilityTimestamp: protoTaskTime,
	}

	persistenceMutableState := s.createPersistenceMutableState(mutableState, completionEvent.GetEventId(), completionEvent.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskRetry, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.fetchHistoryDuration))
	s.mockHistoryRereplicator.On("SendMultiWorkflowHistory",
		primitives.UUIDString(timerTask.GetNamespaceId()), timerTask.GetWorkflowId(),
		primitives.UUIDString(timerTask.GetRunId()), nextEventID,
		primitives.UUIDString(timerTask.GetRunId()), common.EndEventID,
	).Return(nil).Once()
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskRetry, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.discardDuration))
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Equal(ErrTaskDiscarded, err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessWorkflowTimeout_Success() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	di := addDecisionTaskScheduledEvent(mutableState)
	event := addDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventId()
	event = addDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")
	event = addCompleteWorkflowEvent(mutableState, event.GetEventId(), nil)

	protoTaskTime, err := types.TimestampProto(s.now)
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeWorkflowTimeout,
		TimeoutType:         int32(eventpb.TimeoutType_StartToClose),
		VisibilityTimestamp: protoTaskTime,
	}

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventId(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Nil(err)
}

func (s *timerQueueStandbyTaskExecutorSuite) TestProcessRetryTimeout() {

	execution := executionpb.WorkflowExecution{
		WorkflowId: "some random workflow ID",
		RunId:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := newMutableStateBuilderWithReplicationStateWithEventV2(s.mockShard, s.mockShard.GetEventsCache(), s.logger, s.version, execution.GetRunId())
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		execution,
		&historyservice.StartWorkflowExecutionRequest{
			NamespaceId: s.namespaceID,
			StartRequest: &workflowservice.StartWorkflowExecutionRequest{
				WorkflowType:                        &commonpb.WorkflowType{Name: workflowType},
				TaskList:                            &tasklistpb.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: 2,
				TaskStartToCloseTimeoutSeconds:      1,
			},
		},
	)
	s.Nil(err)

	protoTaskTime, err := types.TimestampProto(s.now)
	s.NoError(err)
	timerTask := &persistenceblobs.TimerTaskInfo{
		Version:             s.version,
		NamespaceId:         primitives.MustParseUUID(s.namespaceID),
		WorkflowId:          execution.GetWorkflowId(),
		RunId:               primitives.MustParseUUID(execution.GetRunId()),
		TaskId:              int64(100),
		TaskType:            persistence.TaskTypeActivityRetryTimer,
		TimeoutType:         int32(eventpb.TimeoutType_StartToClose),
		VisibilityTimestamp: protoTaskTime,
	}

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerQueueStandbyTaskExecutor.execute(timerTask, true)
	s.Nil(err)
}

func (s *timerQueueStandbyTaskExecutorSuite) createPersistenceMutableState(
	ms mutableState,
	lastEventID int64,
	lastEventVersion int64,
) *persistence.WorkflowMutableState {

	if ms.GetReplicationState() != nil {
		ms.UpdateReplicationStateLastEventID(lastEventVersion, lastEventID)
	} else if ms.GetVersionHistories() != nil {
		currentVersionHistory, err := ms.GetVersionHistories().GetCurrentVersionHistory()
		s.NoError(err)
		err = currentVersionHistory.AddOrUpdateItem(persistence.NewVersionHistoryItem(
			lastEventID, lastEventVersion,
		))
		s.NoError(err)
	}

	return createMutableState(ms)
}
