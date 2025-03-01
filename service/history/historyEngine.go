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
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/pborman/uuid"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	querypb "go.temporal.io/api/query/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"

	enumsspb "go.temporal.io/server/api/enums/v1"
	historyspb "go.temporal.io/server/api/history/v1"
	"go.temporal.io/server/api/historyservice/v1"
	"go.temporal.io/server/api/matchingservice/v1"
	replicationspb "go.temporal.io/server/api/replication/v1"
	"go.temporal.io/server/client"
	"go.temporal.io/server/common"
	"go.temporal.io/server/common/clock"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/definition"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/serialization"
	"go.temporal.io/server/common/persistence/versionhistory"
	"go.temporal.io/server/common/primitives/timestamp"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/common/searchattribute"
	serviceerrors "go.temporal.io/server/common/serviceerror"
	"go.temporal.io/server/service/history/api"
	"go.temporal.io/server/service/history/api/recordactivitytaskheartbeat"
	"go.temporal.io/server/service/history/api/recordactivitytaskstarted"
	"go.temporal.io/server/service/history/api/requestcancelworkflow"
	"go.temporal.io/server/service/history/api/resetstickytaskqueue"
	respondactivitytaskcandeled "go.temporal.io/server/service/history/api/respondactivitytaskcanceled"
	"go.temporal.io/server/service/history/api/respondactivitytaskcompleted"
	"go.temporal.io/server/service/history/api/respondactivitytaskfailed"
	"go.temporal.io/server/service/history/api/signalwithstartworkflow"
	"go.temporal.io/server/service/history/api/signalworkflow"
	"go.temporal.io/server/service/history/api/startworkflow"
	"go.temporal.io/server/service/history/api/terminateworkflow"
	"go.temporal.io/server/service/history/configs"
	"go.temporal.io/server/service/history/consts"
	"go.temporal.io/server/service/history/events"
	"go.temporal.io/server/service/history/queues"
	"go.temporal.io/server/service/history/replication"
	"go.temporal.io/server/service/history/shard"
	"go.temporal.io/server/service/history/tasks"
	"go.temporal.io/server/service/history/workflow"
	"go.temporal.io/server/service/worker/archiver"
)

const (
	activityCancellationMsgActivityNotStarted = "ACTIVITY_ID_NOT_STARTED"
)

type (
	historyEngineImpl struct {
		status                     int32
		currentClusterName         string
		shard                      shard.Context
		timeSource                 clock.TimeSource
		workflowTaskHandler        workflowTaskHandlerCallbacks
		clusterMetadata            cluster.Metadata
		executionManager           persistence.ExecutionManager
		queueProcessors            map[tasks.Category]queues.Queue
		replicationAckMgr          replication.AckManager
		nDCReplicator              nDCHistoryReplicator
		nDCActivityReplicator      nDCActivityReplicator
		replicationProcessorMgr    common.Daemon
		eventNotifier              events.Notifier
		tokenSerializer            common.TaskTokenSerializer
		metricsClient              metrics.Client
		logger                     log.Logger
		throttledLogger            log.Logger
		config                     *configs.Config
		workflowRebuilder          workflowRebuilder
		workflowResetter           workflowResetter
		sdkClientFactory           sdk.ClientFactory
		eventsReapplier            nDCEventsReapplier
		matchingClient             matchingservice.MatchingServiceClient
		rawMatchingClient          matchingservice.MatchingServiceClient
		replicationDLQHandler      replication.DLQHandler
		searchAttributesValidator  *searchattribute.Validator
		workflowDeleteManager      workflow.DeleteManager
		eventSerializer            serialization.Serializer
		workflowConsistencyChecker api.WorkflowConsistencyChecker
		tracer                     trace.Tracer
	}
)

// NewEngineWithShardContext creates an instance of history engine
func NewEngineWithShardContext(
	shard shard.Context,
	clientBean client.Bean,
	matchingClient matchingservice.MatchingServiceClient,
	sdkClientFactory sdk.ClientFactory,
	eventNotifier events.Notifier,
	config *configs.Config,
	rawMatchingClient matchingservice.MatchingServiceClient,
	workflowCache workflow.Cache,
	archivalClient archiver.Client,
	eventSerializer serialization.Serializer,
	queueProcessorFactories []QueueFactory,
	replicationTaskFetcherFactory replication.TaskFetcherFactory,
	replicationTaskExecutorProvider replication.TaskExecutorProvider,
	workflowConsistencyChecker api.WorkflowConsistencyChecker,
	tracerProvider trace.TracerProvider,
) shard.Engine {
	currentClusterName := shard.GetClusterMetadata().GetCurrentClusterName()

	logger := shard.GetLogger()
	executionManager := shard.GetExecutionManager()

	workflowDeleteManager := workflow.NewDeleteManager(
		shard,
		workflowCache,
		config,
		archivalClient,
		shard.GetTimeSource(),
	)

	historyEngImpl := &historyEngineImpl{
		status:                     common.DaemonStatusInitialized,
		currentClusterName:         currentClusterName,
		shard:                      shard,
		clusterMetadata:            shard.GetClusterMetadata(),
		timeSource:                 shard.GetTimeSource(),
		executionManager:           executionManager,
		tokenSerializer:            common.NewProtoTaskTokenSerializer(),
		logger:                     log.With(logger, tag.ComponentHistoryEngine),
		throttledLogger:            log.With(shard.GetThrottledLogger(), tag.ComponentHistoryEngine),
		metricsClient:              shard.GetMetricsClient(),
		eventNotifier:              eventNotifier,
		config:                     config,
		sdkClientFactory:           sdkClientFactory,
		matchingClient:             matchingClient,
		rawMatchingClient:          rawMatchingClient,
		workflowDeleteManager:      workflowDeleteManager,
		eventSerializer:            eventSerializer,
		workflowConsistencyChecker: workflowConsistencyChecker,
		tracer:                     tracerProvider.Tracer(consts.LibraryName),
	}

	historyEngImpl.queueProcessors = make(map[tasks.Category]queues.Queue)
	for _, factory := range queueProcessorFactories {
		processor := factory.CreateQueue(shard, historyEngImpl, workflowCache)
		historyEngImpl.queueProcessors[processor.Category()] = processor
	}

	historyEngImpl.eventsReapplier = newNDCEventsReapplier(shard.GetMetricsClient(), logger)

	if shard.GetClusterMetadata().IsGlobalNamespaceEnabled() {
		historyEngImpl.replicationAckMgr = replication.NewAckManager(
			shard,
			workflowCache,
			executionManager,
			logger,
		)
		historyEngImpl.nDCReplicator = newNDCHistoryReplicator(
			shard,
			workflowCache,
			historyEngImpl.eventsReapplier,
			logger,
			eventSerializer,
		)
		historyEngImpl.nDCActivityReplicator = newNDCActivityReplicator(
			shard,
			workflowCache,
			logger,
		)
	}
	historyEngImpl.workflowRebuilder = NewWorkflowRebuilder(
		shard,
		workflowCache,
		logger,
	)
	historyEngImpl.workflowResetter = newWorkflowResetter(
		shard,
		workflowCache,
		logger,
	)

	historyEngImpl.searchAttributesValidator = searchattribute.NewValidator(
		shard.GetSearchAttributesProvider(),
		shard.GetSearchAttributesMapper(),
		config.SearchAttributesNumberOfKeysLimit,
		config.SearchAttributesSizeOfValueLimit,
		config.SearchAttributesTotalSizeLimit,
	)

	historyEngImpl.workflowTaskHandler = newWorkflowTaskHandlerCallback(historyEngImpl)
	historyEngImpl.replicationDLQHandler = replication.NewLazyDLQHandler(
		shard,
		workflowDeleteManager,
		workflowCache,
		clientBean,
		replicationTaskExecutorProvider,
	)
	historyEngImpl.replicationProcessorMgr = replication.NewTaskProcessorManager(
		config,
		shard,
		historyEngImpl,
		workflowCache,
		workflowDeleteManager,
		clientBean,
		eventSerializer,
		replicationTaskFetcherFactory,
		replicationTaskExecutorProvider,
	)
	return historyEngImpl
}

// Start will spin up all the components needed to start serving this shard.
// Make sure all the components are loaded lazily so start can return immediately.  This is important because
// ShardController calls start sequentially for all the shards for a given host during startup.
func (e *historyEngineImpl) Start() {
	if !atomic.CompareAndSwapInt32(
		&e.status,
		common.DaemonStatusInitialized,
		common.DaemonStatusStarted,
	) {
		return
	}

	e.logger.Info("", tag.LifeCycleStarting)
	defer e.logger.Info("", tag.LifeCycleStarted)

	for _, queueProcessor := range e.queueProcessors {
		queueProcessor.Start()
	}
	e.replicationProcessorMgr.Start()

	// failover callback will try to create a failover queue processor to scan all inflight tasks
	// if domain needs to be failovered. However, in the multicursor queue logic, the scan range
	// can't be retrieved before the processor is started. If failover callback is registered
	// before queue processor is started, it may result in a deadline as to create the failover queue,
	// queue processor need to be started.
	//
	// Ideally, when both timer and transfer queues enabled single cursor mode, we don't have to register
	// the callback. However, currently namespace migration is relying on the callback to UpdateHandoverNamespaces
	e.registerNamespaceFailoverCallback()
}

// Stop the service.
func (e *historyEngineImpl) Stop() {
	if !atomic.CompareAndSwapInt32(
		&e.status,
		common.DaemonStatusStarted,
		common.DaemonStatusStopped,
	) {
		return
	}

	e.logger.Info("", tag.LifeCycleStopping)
	defer e.logger.Info("", tag.LifeCycleStopped)

	for _, queueProcessor := range e.queueProcessors {
		queueProcessor.Stop()
	}
	e.replicationProcessorMgr.Stop()
	// unset the failover callback
	e.shard.GetNamespaceRegistry().UnregisterNamespaceChangeCallback(e)
}

func (e *historyEngineImpl) registerNamespaceFailoverCallback() {

	// NOTE: READ BEFORE MODIFICATION
	//
	// Tasks, e.g. transfer tasks and timer tasks, are created when holding the shard lock
	// meaning tasks -> release of shard lock
	//
	// Namespace change notification follows the following steps, order matters
	// 1. lock all task processing.
	// 2. namespace changes visible to everyone (Note: lock of task processing prevents task processing logic seeing the namespace changes).
	// 3. failover min and max task levels are calculated, then update to shard.
	// 4. failover start & task processing unlock & shard namespace version notification update. (order does not matter for this discussion)
	//
	// The above guarantees that task created during the failover will be processed.
	// If the task is created after namespace change:
	// 		then active processor will handle it. (simple case)
	// If the task is created before namespace change:
	//		task -> release of shard lock
	//		failover min / max task levels calculated & updated to shard (using shard lock) -> failover start
	// above 2 guarantees that failover start is after persistence of the task.

	failoverPredicate := func(shardNotificationVersion int64, nextNamespace *namespace.Namespace, action func()) {
		namespaceFailoverNotificationVersion := nextNamespace.FailoverNotificationVersion()
		namespaceActiveCluster := nextNamespace.ActiveClusterName()

		// +1 in the following check as the version in shard is max notification version +1.
		// Need to run action() when namespaceFailoverNotificationVersion+1 == shardNotificationVersion
		// as we don't know if the failover queue execution for that notification version is
		// completed or not.
		//
		// NOTE: theoretically we need to get rid of the check on shardNotificationVersion, as
		// we have no idea if the failover queue for any notification version below that is completed
		// or not. However, removing that will cause more load upon shard reload.
		// So here assume failover queue processor for notification version < X-1 is completed if
		// shard notification version is X.

		if nextNamespace.IsGlobalNamespace() &&
			nextNamespace.ReplicationPolicy() == namespace.ReplicationPolicyMultiCluster &&
			namespaceFailoverNotificationVersion+1 >= shardNotificationVersion &&
			namespaceActiveCluster == e.currentClusterName {
			action()
		}
	}

	// first set the failover callback
	e.shard.GetNamespaceRegistry().RegisterNamespaceChangeCallback(
		e,
		0, /* always want callback so UpdateHandoverNamespaces() can be called after shard reload */
		func() {
			for _, queueProcessor := range e.queueProcessors {
				queueProcessor.LockTaskProcessing()
			}
		},
		func(prevNamespaces []*namespace.Namespace, nextNamespaces []*namespace.Namespace) {
			defer func() {
				for _, queueProcessor := range e.queueProcessors {
					queueProcessor.UnlockTaskProcessing()
				}
			}()

			if len(nextNamespaces) == 0 {
				return
			}

			if e.shard.GetClusterMetadata().IsGlobalNamespaceEnabled() {
				maxTaskID, _ := e.replicationAckMgr.GetMaxTaskInfo()
				e.shard.UpdateHandoverNamespaces(nextNamespaces, maxTaskID)
			}

			newNotificationVersion := nextNamespaces[len(nextNamespaces)-1].NotificationVersion() + 1
			shardNotificationVersion := e.shard.GetNamespaceNotificationVersion()

			// 1. We can't return when newNotificationVersion == shardNotificationVersion
			// since we don't know if the previous failover queue processing has finished or not
			// 2. We can return when newNotificationVersion < shardNotificationVersion. But the check
			// is basically the same as the check in failover predicate. Because
			// failoverNotificationVersion + 1 <= NotificationVersion + 1 = newNotificationVersion,
			// there's no notification version can make
			// newNotificationVersion < shardNotificationVersion and
			// failoverNotificationVersion + 1 >= shardNotificationVersion are true at the same time
			// Meaning if the check decides to return, no namespace will pass the failover predicate.

			failoverNamespaceIDs := map[string]struct{}{}
			for _, nextNamespace := range nextNamespaces {
				failoverPredicate(shardNotificationVersion, nextNamespace, func() {
					failoverNamespaceIDs[nextNamespace.ID().String()] = struct{}{}
				})
			}

			if len(failoverNamespaceIDs) > 0 {
				e.logger.Info("Namespace Failover Start.", tag.WorkflowNamespaceIDs(failoverNamespaceIDs))

				for _, queueProcessor := range e.queueProcessors {
					queueProcessor.FailoverNamespace(failoverNamespaceIDs)
				}

				// the fake tasks will not be actually used, we just need to make sure
				// its length > 0 and has correct timestamp, to trigger a db scan
				now := e.shard.GetTimeSource().Now()
				fakeTasks := make(map[tasks.Category][]tasks.Task)
				for category := range e.queueProcessors {
					fakeTasks[category] = []tasks.Task{tasks.NewFakeTask(definition.WorkflowKey{}, category, now)}
				}
				e.NotifyNewTasks(e.currentClusterName, fakeTasks)
			}

			_ = e.shard.UpdateNamespaceNotificationVersion(newNotificationVersion)
		},
	)
}

// StartWorkflowExecution starts a workflow execution
// Consistency guarantee: always write
func (e *historyEngineImpl) StartWorkflowExecution(
	ctx context.Context,
	startRequest *historyservice.StartWorkflowExecutionRequest,
) (resp *historyservice.StartWorkflowExecutionResponse, retError error) {
	return startworkflow.Invoke(ctx, startRequest, e.shard, e.workflowConsistencyChecker)
}

// GetMutableState retrieves the mutable state of the workflow execution
func (e *historyEngineImpl) GetMutableState(
	ctx context.Context,
	request *historyservice.GetMutableStateRequest,
) (*historyservice.GetMutableStateResponse, error) {

	return e.getMutableStateOrPolling(ctx, request)
}

// PollMutableState retrieves the mutable state of the workflow execution with long polling
func (e *historyEngineImpl) PollMutableState(
	ctx context.Context,
	request *historyservice.PollMutableStateRequest,
) (*historyservice.PollMutableStateResponse, error) {

	response, err := e.getMutableStateOrPolling(ctx, &historyservice.GetMutableStateRequest{
		NamespaceId:         request.GetNamespaceId(),
		Execution:           request.Execution,
		ExpectedNextEventId: request.ExpectedNextEventId,
		CurrentBranchToken:  request.CurrentBranchToken,
	})

	if err != nil {
		return nil, err
	}
	return &historyservice.PollMutableStateResponse{
		Execution:                             response.Execution,
		WorkflowType:                          response.WorkflowType,
		NextEventId:                           response.NextEventId,
		PreviousStartedEventId:                response.PreviousStartedEventId,
		LastFirstEventId:                      response.LastFirstEventId,
		LastFirstEventTxnId:                   response.LastFirstEventTxnId,
		TaskQueue:                             response.TaskQueue,
		StickyTaskQueue:                       response.StickyTaskQueue,
		StickyTaskQueueScheduleToStartTimeout: response.StickyTaskQueueScheduleToStartTimeout,
		CurrentBranchToken:                    response.CurrentBranchToken,
		VersionHistories:                      response.VersionHistories,
		WorkflowState:                         response.WorkflowState,
		WorkflowStatus:                        response.WorkflowStatus,
		FirstExecutionRunId:                   response.FirstExecutionRunId,
	}, nil
}

func (e *historyEngineImpl) getMutableStateOrPolling(
	ctx context.Context,
	request *historyservice.GetMutableStateRequest,
) (*historyservice.GetMutableStateResponse, error) {

	namespaceID := namespace.ID(request.GetNamespaceId())
	err := api.ValidateNamespaceUUID(namespaceID)
	if err != nil {
		return nil, err
	}

	if len(request.Execution.RunId) == 0 {
		request.Execution.RunId, err = e.workflowConsistencyChecker.GetCurrentRunID(
			ctx,
			request.NamespaceId,
			request.Execution.WorkflowId,
		)
		if err != nil {
			return nil, err
		}
	}
	workflowKey := definition.NewWorkflowKey(
		request.NamespaceId,
		request.Execution.WorkflowId,
		request.Execution.RunId,
	)
	response, err := e.getMutableState(ctx, workflowKey)
	if err != nil {
		return nil, err
	}
	if request.CurrentBranchToken == nil {
		request.CurrentBranchToken = response.CurrentBranchToken
	}
	if !bytes.Equal(request.CurrentBranchToken, response.CurrentBranchToken) {
		return nil, serviceerrors.NewCurrentBranchChanged(response.CurrentBranchToken, request.CurrentBranchToken)
	}

	// expectedNextEventID is 0 when caller want to get the current next event ID without blocking
	expectedNextEventID := common.FirstEventID
	if request.ExpectedNextEventId != common.EmptyEventID {
		expectedNextEventID = request.GetExpectedNextEventId()
	}

	// if caller decide to long poll on workflow execution
	// and the event ID we are looking for is smaller than current next event ID
	if expectedNextEventID >= response.GetNextEventId() && response.GetWorkflowStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		subscriberID, channel, err := e.eventNotifier.WatchHistoryEvent(workflowKey)
		if err != nil {
			return nil, err
		}
		defer func() { _ = e.eventNotifier.UnwatchHistoryEvent(workflowKey, subscriberID) }()
		// check again in case the next event ID is updated
		response, err = e.getMutableState(ctx, workflowKey)
		if err != nil {
			return nil, err
		}
		// check again if the current branch token changed
		if !bytes.Equal(request.CurrentBranchToken, response.CurrentBranchToken) {
			return nil, serviceerrors.NewCurrentBranchChanged(response.CurrentBranchToken, request.CurrentBranchToken)
		}
		if expectedNextEventID < response.GetNextEventId() || response.GetWorkflowStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
			return response, nil
		}

		namespaceRegistry, err := e.shard.GetNamespaceRegistry().GetNamespaceByID(namespaceID)
		if err != nil {
			return nil, err
		}
		timer := time.NewTimer(e.shard.GetConfig().LongPollExpirationInterval(namespaceRegistry.Name().String()))
		defer timer.Stop()
		for {
			select {
			case event := <-channel:
				response.LastFirstEventId = event.LastFirstEventID
				response.LastFirstEventTxnId = event.LastFirstEventTxnID
				response.NextEventId = event.NextEventID
				response.PreviousStartedEventId = event.PreviousStartedEventID
				response.WorkflowState = event.WorkflowState
				response.WorkflowStatus = event.WorkflowStatus
				if !bytes.Equal(request.CurrentBranchToken, event.CurrentBranchToken) {
					return nil, serviceerrors.NewCurrentBranchChanged(event.CurrentBranchToken, request.CurrentBranchToken)
				}
				if expectedNextEventID < response.GetNextEventId() || response.GetWorkflowStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
					return response, nil
				}
			case <-timer.C:
				return response, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	return response, nil
}

func (e *historyEngineImpl) QueryWorkflow(
	ctx context.Context,
	request *historyservice.QueryWorkflowRequest,
) (_ *historyservice.QueryWorkflowResponse, retErr error) {

	scope := e.metricsClient.Scope(metrics.HistoryQueryWorkflowScope)
	namespaceID := namespace.ID(request.GetNamespaceId())
	err := api.ValidateNamespaceUUID(namespaceID)
	if err != nil {
		return nil, err
	}
	nsEntry, err := e.shard.GetNamespaceRegistry().GetNamespaceByID(namespaceID)
	if err != nil {
		return nil, err
	}

	if len(request.Request.Execution.RunId) == 0 {
		request.Request.Execution.RunId, err = e.workflowConsistencyChecker.GetCurrentRunID(
			ctx,
			request.NamespaceId,
			request.Request.Execution.WorkflowId,
		)
		if err != nil {
			return nil, err
		}
	}
	workflowKey := definition.NewWorkflowKey(
		request.NamespaceId,
		request.Request.Execution.WorkflowId,
		request.Request.Execution.RunId,
	)
	weCtx, err := e.workflowConsistencyChecker.GetWorkflowContext(
		ctx,
		nil,
		api.BypassMutableStateConsistencyPredicate,
		workflowKey,
	)
	if err != nil {
		return nil, err
	}
	defer func() { weCtx.GetReleaseFn()(retErr) }()

	req := request.GetRequest()
	_, mutableStateStatus := weCtx.GetMutableState().GetWorkflowStateStatus()
	if mutableStateStatus != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING && req.QueryRejectCondition != enumspb.QUERY_REJECT_CONDITION_NONE {
		notOpenReject := req.GetQueryRejectCondition() == enumspb.QUERY_REJECT_CONDITION_NOT_OPEN
		notCompletedCleanlyReject := req.GetQueryRejectCondition() == enumspb.QUERY_REJECT_CONDITION_NOT_COMPLETED_CLEANLY && mutableStateStatus != enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED
		if notOpenReject || notCompletedCleanlyReject {
			return &historyservice.QueryWorkflowResponse{
				Response: &workflowservice.QueryWorkflowResponse{
					QueryRejected: &querypb.QueryRejected{
						Status: mutableStateStatus,
					},
				},
			}, nil
		}
	}

	mutableState := weCtx.GetMutableState()
	if !mutableState.HasProcessedOrPendingWorkflowTask() {
		// workflow has no workflow task ever scheduled, this usually is due to firstWorkflowTaskBackoff (cron / retry)
		// in this case, don't buffer the query, because it is almost certain the query will time out.
		return nil, consts.ErrWorkflowTaskNotScheduled
	}

	// There are two ways in which queries get dispatched to workflow worker. First, queries can be dispatched on workflow tasks.
	// These workflow tasks potentially contain new events and queries. The events are treated as coming before the query in time.
	// The second way in which queries are dispatched to workflow worker is directly through matching; in this approach queries can be
	// dispatched to workflow worker immediately even if there are outstanding events that came before the query. The following logic
	// is used to determine if a query can be safely dispatched directly through matching or must be dispatched on a workflow task.
	//
	// Precondition to dispatch query directly to matching is workflow has at least one WorkflowTaskStarted event. Otherwise, sdk would panic.
	if mutableState.GetPreviousStartedEventID() != common.EmptyEventID {
		// There are three cases in which a query can be dispatched directly through matching safely, without violating strong consistency level:
		// 1. the namespace is not active, in this case history is immutable so a query dispatched at any time is consistent
		// 2. the workflow is not running, whenever a workflow is not running dispatching query directly is consistent
		// 3. if there is no pending or started workflow tasks it means no events came before query arrived, so its safe to dispatch directly
		safeToDispatchDirectly := !nsEntry.ActiveInCluster(e.clusterMetadata.GetCurrentClusterName()) ||
			!mutableState.IsWorkflowExecutionRunning() ||
			(!mutableState.HasPendingWorkflowTask() && !mutableState.HasInFlightWorkflowTask())
		if safeToDispatchDirectly {
			msResp, err := e.mutableStateToGetResponse(mutableState)
			if err != nil {
				return nil, err
			}
			weCtx.GetReleaseFn()(nil)
			req.Execution.RunId = msResp.Execution.RunId
			return e.queryDirectlyThroughMatching(ctx, msResp, request.GetNamespaceId(), req, scope)
		}
	}

	// If we get here it means query could not be dispatched through matching directly, so it must block
	// until either an result has been obtained on a workflow task response or until it is safe to dispatch directly through matching.
	sw := scope.StartTimer(metrics.WorkflowTaskQueryLatency)
	defer sw.Stop()
	queryReg := mutableState.GetQueryRegistry()
	if len(queryReg.GetBufferedIDs()) >= e.config.MaxBufferedQueryCount() {
		scope.IncCounter(metrics.QueryBufferExceededCount)
		return nil, consts.ErrConsistentQueryBufferExceeded
	}
	queryID, completionCh := queryReg.BufferQuery(req.GetQuery())
	defer queryReg.RemoveQuery(queryID)
	weCtx.GetReleaseFn()(nil)
	select {
	case <-completionCh:
		completionState, err := queryReg.GetCompletionState(queryID)
		if err != nil {
			scope.IncCounter(metrics.QueryRegistryInvalidStateCount)
			return nil, err
		}
		switch completionState.Type {
		case workflow.QueryCompletionTypeSucceeded:
			result := completionState.Result
			switch result.GetResultType() {
			case enumspb.QUERY_RESULT_TYPE_ANSWERED:
				return &historyservice.QueryWorkflowResponse{
					Response: &workflowservice.QueryWorkflowResponse{
						QueryResult: result.GetAnswer(),
					},
				}, nil
			case enumspb.QUERY_RESULT_TYPE_FAILED:
				return nil, serviceerror.NewQueryFailed(result.GetErrorMessage())
			default:
				scope.IncCounter(metrics.QueryRegistryInvalidStateCount)
				return nil, consts.ErrQueryEnteredInvalidState
			}
		case workflow.QueryCompletionTypeUnblocked:
			msResp, err := e.getMutableState(ctx, workflowKey)
			if err != nil {
				return nil, err
			}
			req.Execution.RunId = msResp.Execution.RunId
			return e.queryDirectlyThroughMatching(ctx, msResp, request.GetNamespaceId(), req, scope)
		case workflow.QueryCompletionTypeFailed:
			return nil, completionState.Err
		default:
			scope.IncCounter(metrics.QueryRegistryInvalidStateCount)
			return nil, consts.ErrQueryEnteredInvalidState
		}
	case <-ctx.Done():
		scope.IncCounter(metrics.ConsistentQueryTimeoutCount)
		return nil, ctx.Err()
	}
}

func (e *historyEngineImpl) queryDirectlyThroughMatching(
	ctx context.Context,
	msResp *historyservice.GetMutableStateResponse,
	namespaceID string,
	queryRequest *workflowservice.QueryWorkflowRequest,
	scope metrics.Scope,
) (*historyservice.QueryWorkflowResponse, error) {

	sw := scope.StartTimer(metrics.DirectQueryDispatchLatency)
	defer sw.Stop()

	if msResp.GetIsStickyTaskQueueEnabled() &&
		len(msResp.GetStickyTaskQueue().GetName()) != 0 &&
		e.config.EnableStickyQuery(queryRequest.GetNamespace()) {

		stickyMatchingRequest := &matchingservice.QueryWorkflowRequest{
			NamespaceId:  namespaceID,
			QueryRequest: queryRequest,
			TaskQueue:    msResp.GetStickyTaskQueue(),
		}

		// using a clean new context in case customer provide a context which has
		// a really short deadline, causing we clear the stickiness
		stickyContext, cancel := context.WithTimeout(context.Background(), timestamp.DurationValue(msResp.GetStickyTaskQueueScheduleToStartTimeout()))
		stickyStopWatch := scope.StartTimer(metrics.DirectQueryDispatchStickyLatency)
		matchingResp, err := e.rawMatchingClient.QueryWorkflow(stickyContext, stickyMatchingRequest)
		stickyStopWatch.Stop()
		cancel()
		if err == nil {
			scope.IncCounter(metrics.DirectQueryDispatchStickySuccessCount)
			return &historyservice.QueryWorkflowResponse{
				Response: &workflowservice.QueryWorkflowResponse{
					QueryResult:   matchingResp.GetQueryResult(),
					QueryRejected: matchingResp.GetQueryRejected(),
				}}, nil
		}
		if !common.IsContextDeadlineExceededErr(err) && !common.IsContextCanceledErr(err) && !common.IsStickyWorkerUnavailable(err) {
			return nil, err
		}
		if msResp.GetWorkflowStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
			resetContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			clearStickinessStopWatch := scope.StartTimer(metrics.DirectQueryDispatchClearStickinessLatency)
			_, err := e.ResetStickyTaskQueue(resetContext, &historyservice.ResetStickyTaskQueueRequest{
				NamespaceId: namespaceID,
				Execution:   queryRequest.GetExecution(),
			})
			clearStickinessStopWatch.Stop()
			cancel()
			if err != nil && err != consts.ErrWorkflowCompleted {
				return nil, err
			}
			scope.IncCounter(metrics.DirectQueryDispatchClearStickinessSuccessCount)
		}
	}

	if err := common.IsValidContext(ctx); err != nil {
		scope.IncCounter(metrics.DirectQueryDispatchTimeoutBeforeNonStickyCount)
		return nil, err
	}

	nonStickyMatchingRequest := &matchingservice.QueryWorkflowRequest{
		NamespaceId:  namespaceID,
		QueryRequest: queryRequest,
		TaskQueue:    msResp.TaskQueue,
	}

	nonStickyStopWatch := scope.StartTimer(metrics.DirectQueryDispatchNonStickyLatency)
	matchingResp, err := e.matchingClient.QueryWorkflow(ctx, nonStickyMatchingRequest)
	nonStickyStopWatch.Stop()
	if err != nil {
		return nil, err
	}
	scope.IncCounter(metrics.DirectQueryDispatchNonStickySuccessCount)
	return &historyservice.QueryWorkflowResponse{
		Response: &workflowservice.QueryWorkflowResponse{
			QueryResult:   matchingResp.GetQueryResult(),
			QueryRejected: matchingResp.GetQueryRejected(),
		}}, err
}

func (e *historyEngineImpl) getMutableState(
	ctx context.Context,
	workflowKey definition.WorkflowKey,
) (_ *historyservice.GetMutableStateResponse, retError error) {

	if len(workflowKey.RunID) == 0 {
		return nil, serviceerror.NewInternal(fmt.Sprintf(
			"getMutableState encountered empty run ID: %v", workflowKey,
		))
	}

	weCtx, err := e.workflowConsistencyChecker.GetWorkflowContext(
		ctx,
		nil,
		api.BypassMutableStateConsistencyPredicate,
		workflowKey,
	)
	if err != nil {
		return nil, err
	}
	defer func() { weCtx.GetReleaseFn()(retError) }()

	mutableState, err := weCtx.GetContext().LoadMutableState(ctx)
	if err != nil {
		return nil, err
	}
	return e.mutableStateToGetResponse(mutableState)
}

func (e *historyEngineImpl) mutableStateToGetResponse(
	mutableState workflow.MutableState,
) (_ *historyservice.GetMutableStateResponse, retError error) {
	currentBranchToken, err := mutableState.GetCurrentBranchToken()
	if err != nil {
		return nil, err
	}

	executionInfo := mutableState.GetExecutionInfo()
	workflowState, workflowStatus := mutableState.GetWorkflowStateStatus()
	lastFirstEventID, lastFirstEventTxnID := mutableState.GetLastFirstEventIDTxnID()
	return &historyservice.GetMutableStateResponse{
		Execution: &commonpb.WorkflowExecution{
			WorkflowId: mutableState.GetExecutionInfo().WorkflowId,
			RunId:      mutableState.GetExecutionState().RunId,
		},
		WorkflowType:           &commonpb.WorkflowType{Name: executionInfo.WorkflowTypeName},
		LastFirstEventId:       lastFirstEventID,
		LastFirstEventTxnId:    lastFirstEventTxnID,
		NextEventId:            mutableState.GetNextEventID(),
		PreviousStartedEventId: mutableState.GetPreviousStartedEventID(),
		TaskQueue: &taskqueuepb.TaskQueue{
			Name: executionInfo.TaskQueue,
			Kind: enumspb.TASK_QUEUE_KIND_NORMAL,
		},
		StickyTaskQueue: &taskqueuepb.TaskQueue{
			Name: executionInfo.StickyTaskQueue,
			Kind: enumspb.TASK_QUEUE_KIND_STICKY,
		},
		StickyTaskQueueScheduleToStartTimeout: executionInfo.StickyScheduleToStartTimeout,
		CurrentBranchToken:                    currentBranchToken,
		WorkflowState:                         workflowState,
		WorkflowStatus:                        workflowStatus,
		IsStickyTaskQueueEnabled:              mutableState.IsStickyTaskQueueEnabled(),
		VersionHistories: versionhistory.CopyVersionHistories(
			mutableState.GetExecutionInfo().GetVersionHistories(),
		),
		FirstExecutionRunId: executionInfo.FirstExecutionRunId,
	}, nil
}

func (e *historyEngineImpl) DescribeMutableState(
	ctx context.Context,
	request *historyservice.DescribeMutableStateRequest,
) (response *historyservice.DescribeMutableStateResponse, retError error) {

	namespaceID := namespace.ID(request.GetNamespaceId())
	err := api.ValidateNamespaceUUID(namespaceID)
	if err != nil {
		return nil, err
	}

	weCtx, err := e.workflowConsistencyChecker.GetWorkflowContext(
		ctx,
		nil,
		api.BypassMutableStateConsistencyPredicate,
		definition.NewWorkflowKey(
			request.NamespaceId,
			request.Execution.WorkflowId,
			request.Execution.RunId,
		),
	)
	if err != nil {
		return nil, err
	}
	defer func() { weCtx.GetReleaseFn()(retError) }()

	response = &historyservice.DescribeMutableStateResponse{}

	if weCtx.GetContext().(*workflow.ContextImpl).MutableState != nil {
		msb := weCtx.GetContext().(*workflow.ContextImpl).MutableState
		response.CacheMutableState = msb.CloneToProto()
	}

	// clear mutable state to force reload from persistence. This API returns both cached and persisted version.
	weCtx.GetContext().Clear()
	mutableState, err := weCtx.GetContext().LoadMutableState(ctx)
	if err != nil {
		return nil, err
	}

	response.DatabaseMutableState = mutableState.CloneToProto()
	return response, nil
}

// ResetStickyTaskQueue reset the volatile information in mutable state of a given workflow.
// Volatile information are the information related to client, such as:
// 1. StickyTaskQueue
// 2. StickyScheduleToStartTimeout
func (e *historyEngineImpl) ResetStickyTaskQueue(
	ctx context.Context,
	resetRequest *historyservice.ResetStickyTaskQueueRequest,
) (*historyservice.ResetStickyTaskQueueResponse, error) {
	return resetstickytaskqueue.Invoke(ctx, resetRequest, e.shard, e.workflowConsistencyChecker)
}

// DescribeWorkflowExecution returns information about the specified workflow execution.
func (e *historyEngineImpl) DescribeWorkflowExecution(
	ctx context.Context,
	request *historyservice.DescribeWorkflowExecutionRequest,
) (_ *historyservice.DescribeWorkflowExecutionResponse, retError error) {

	namespaceID := namespace.ID(request.GetNamespaceId())
	err := api.ValidateNamespaceUUID(namespaceID)
	if err != nil {
		return nil, err
	}

	weCtx, err := e.workflowConsistencyChecker.GetWorkflowContext(
		ctx,
		nil,
		api.BypassMutableStateConsistencyPredicate,
		definition.NewWorkflowKey(
			request.NamespaceId,
			request.Request.Execution.WorkflowId,
			request.Request.Execution.RunId,
		),
	)
	if err != nil {
		return nil, err
	}
	defer func() { weCtx.GetReleaseFn()(retError) }()

	mutableState := weCtx.GetMutableState()
	executionInfo := mutableState.GetExecutionInfo()
	executionState := mutableState.GetExecutionState()
	result := &historyservice.DescribeWorkflowExecutionResponse{
		ExecutionConfig: &workflowpb.WorkflowExecutionConfig{
			TaskQueue: &taskqueuepb.TaskQueue{
				Name: executionInfo.TaskQueue,
				Kind: enumspb.TASK_QUEUE_KIND_NORMAL,
			},
			WorkflowExecutionTimeout:   executionInfo.WorkflowExecutionTimeout,
			WorkflowRunTimeout:         executionInfo.WorkflowRunTimeout,
			DefaultWorkflowTaskTimeout: executionInfo.DefaultWorkflowTaskTimeout,
		},
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{
			Execution: &commonpb.WorkflowExecution{
				WorkflowId: executionInfo.WorkflowId,
				RunId:      executionState.RunId,
			},
			Type:                 &commonpb.WorkflowType{Name: executionInfo.WorkflowTypeName},
			StartTime:            executionInfo.StartTime,
			Status:               executionState.Status,
			HistoryLength:        mutableState.GetNextEventID() - common.FirstEventID,
			ExecutionTime:        executionInfo.ExecutionTime,
			Memo:                 &commonpb.Memo{Fields: executionInfo.Memo},
			SearchAttributes:     &commonpb.SearchAttributes{IndexedFields: executionInfo.SearchAttributes},
			AutoResetPoints:      executionInfo.AutoResetPoints,
			TaskQueue:            executionInfo.TaskQueue,
			StateTransitionCount: executionInfo.StateTransitionCount,
		},
	}

	if executionInfo.ParentRunId != "" {
		result.WorkflowExecutionInfo.ParentExecution = &commonpb.WorkflowExecution{
			WorkflowId: executionInfo.ParentWorkflowId,
			RunId:      executionInfo.ParentRunId,
		}
		result.WorkflowExecutionInfo.ParentNamespaceId = executionInfo.ParentNamespaceId
	}
	if executionState.State == enumsspb.WORKFLOW_EXECUTION_STATE_COMPLETED {
		// for closed workflow
		result.WorkflowExecutionInfo.Status = executionState.Status
		closeTime, err := mutableState.GetWorkflowCloseTime(ctx)
		if err != nil {
			return nil, err
		}
		result.WorkflowExecutionInfo.CloseTime = closeTime
	}

	if len(mutableState.GetPendingActivityInfos()) > 0 {
		for _, ai := range mutableState.GetPendingActivityInfos() {
			p := &workflowpb.PendingActivityInfo{
				ActivityId: ai.ActivityId,
			}
			if ai.CancelRequested {
				p.State = enumspb.PENDING_ACTIVITY_STATE_CANCEL_REQUESTED
			} else if ai.StartedEventId != common.EmptyEventID {
				p.State = enumspb.PENDING_ACTIVITY_STATE_STARTED
			} else {
				p.State = enumspb.PENDING_ACTIVITY_STATE_SCHEDULED
			}
			if !timestamp.TimeValue(ai.LastHeartbeatUpdateTime).IsZero() {
				p.LastHeartbeatTime = ai.LastHeartbeatUpdateTime
				p.HeartbeatDetails = ai.LastHeartbeatDetails
			}
			// TODO: move to mutable state instead of loading it from event
			scheduledEvent, err := mutableState.GetActivityScheduledEvent(ctx, ai.ScheduledEventId)
			if err != nil {
				return nil, err
			}
			p.ActivityType = scheduledEvent.GetActivityTaskScheduledEventAttributes().ActivityType
			if p.State == enumspb.PENDING_ACTIVITY_STATE_SCHEDULED {
				p.ScheduledTime = ai.ScheduledTime
			} else {
				p.LastStartedTime = ai.StartedTime
			}
			p.LastWorkerIdentity = ai.StartedIdentity
			if ai.HasRetryPolicy {
				p.Attempt = ai.Attempt
				p.ExpirationTime = ai.RetryExpirationTime
				if ai.RetryMaximumAttempts != 0 {
					p.MaximumAttempts = ai.RetryMaximumAttempts
				}
				if ai.RetryLastFailure != nil {
					p.LastFailure = ai.RetryLastFailure
				}
				if p.LastWorkerIdentity == "" && ai.RetryLastWorkerIdentity != "" {
					p.LastWorkerIdentity = ai.RetryLastWorkerIdentity
				}
			} else {
				p.Attempt = 1
			}
			result.PendingActivities = append(result.PendingActivities, p)
		}
	}

	if len(mutableState.GetPendingChildExecutionInfos()) > 0 {
		for _, ch := range mutableState.GetPendingChildExecutionInfos() {
			p := &workflowpb.PendingChildExecutionInfo{
				WorkflowId:        ch.StartedWorkflowId,
				RunId:             ch.StartedRunId,
				WorkflowTypeName:  ch.WorkflowTypeName,
				InitiatedId:       ch.InitiatedEventId,
				ParentClosePolicy: ch.ParentClosePolicy,
			}
			result.PendingChildren = append(result.PendingChildren, p)
		}
	}

	if pendingWorkflowTask, ok := mutableState.GetPendingWorkflowTask(); ok {
		result.PendingWorkflowTask = &workflowpb.PendingWorkflowTaskInfo{
			State:                 enumspb.PENDING_WORKFLOW_TASK_STATE_SCHEDULED,
			ScheduledTime:         pendingWorkflowTask.ScheduledTime,
			OriginalScheduledTime: pendingWorkflowTask.OriginalScheduledTime,
			Attempt:               pendingWorkflowTask.Attempt,
		}
		if pendingWorkflowTask.StartedEventID != common.EmptyEventID {
			result.PendingWorkflowTask.State = enumspb.PENDING_WORKFLOW_TASK_STATE_STARTED
			result.PendingWorkflowTask.StartedTime = pendingWorkflowTask.StartedTime
		}
	}

	return result, nil
}

func (e *historyEngineImpl) RecordActivityTaskStarted(
	ctx context.Context,
	request *historyservice.RecordActivityTaskStartedRequest,
) (*historyservice.RecordActivityTaskStartedResponse, error) {
	return recordactivitytaskstarted.Invoke(ctx, request, e.shard, e.workflowConsistencyChecker)
}

// ScheduleWorkflowTask schedules a workflow task if no outstanding workflow task found
func (e *historyEngineImpl) ScheduleWorkflowTask(
	ctx context.Context,
	req *historyservice.ScheduleWorkflowTaskRequest,
) error {
	return e.workflowTaskHandler.handleWorkflowTaskScheduled(ctx, req)
}

func (e *historyEngineImpl) VerifyFirstWorkflowTaskScheduled(
	ctx context.Context,
	request *historyservice.VerifyFirstWorkflowTaskScheduledRequest,
) (retError error) {
	return e.workflowTaskHandler.verifyFirstWorkflowTaskScheduled(ctx, request)
}

// RecordWorkflowTaskStarted starts a workflow task
func (e *historyEngineImpl) RecordWorkflowTaskStarted(
	ctx context.Context,
	request *historyservice.RecordWorkflowTaskStartedRequest,
) (*historyservice.RecordWorkflowTaskStartedResponse, error) {
	return e.workflowTaskHandler.handleWorkflowTaskStarted(ctx, request)
}

// RespondWorkflowTaskCompleted completes a workflow task
func (e *historyEngineImpl) RespondWorkflowTaskCompleted(
	ctx context.Context,
	req *historyservice.RespondWorkflowTaskCompletedRequest,
) (*historyservice.RespondWorkflowTaskCompletedResponse, error) {
	return e.workflowTaskHandler.handleWorkflowTaskCompleted(ctx, req)
}

// RespondWorkflowTaskFailed fails a workflow task
func (e *historyEngineImpl) RespondWorkflowTaskFailed(
	ctx context.Context,
	req *historyservice.RespondWorkflowTaskFailedRequest,
) error {
	return e.workflowTaskHandler.handleWorkflowTaskFailed(ctx, req)
}

// RespondActivityTaskCompleted completes an activity task.
func (e *historyEngineImpl) RespondActivityTaskCompleted(
	ctx context.Context,
	req *historyservice.RespondActivityTaskCompletedRequest,
) (*historyservice.RespondActivityTaskCompletedResponse, error) {
	return respondactivitytaskcompleted.Invoke(ctx, req, e.shard, e.workflowConsistencyChecker)
}

// RespondActivityTaskFailed completes an activity task failure.
func (e *historyEngineImpl) RespondActivityTaskFailed(
	ctx context.Context,
	req *historyservice.RespondActivityTaskFailedRequest,
) (*historyservice.RespondActivityTaskFailedResponse, error) {
	return respondactivitytaskfailed.Invoke(ctx, req, e.shard, e.workflowConsistencyChecker)
}

// RespondActivityTaskCanceled completes an activity task failure.
func (e *historyEngineImpl) RespondActivityTaskCanceled(
	ctx context.Context,
	req *historyservice.RespondActivityTaskCanceledRequest,
) (*historyservice.RespondActivityTaskCanceledResponse, error) {
	return respondactivitytaskcandeled.Invoke(ctx, req, e.shard, e.workflowConsistencyChecker)
}

// RecordActivityTaskHeartbeat records an hearbeat for a task.
// This method can be used for two purposes.
// - For reporting liveness of the activity.
// - For reporting progress of the activity, this can be done even if the liveness is not configured.
func (e *historyEngineImpl) RecordActivityTaskHeartbeat(
	ctx context.Context,
	req *historyservice.RecordActivityTaskHeartbeatRequest,
) (*historyservice.RecordActivityTaskHeartbeatResponse, error) {
	return recordactivitytaskheartbeat.Invoke(ctx, req, e.shard, e.workflowConsistencyChecker)
}

// RequestCancelWorkflowExecution records request cancellation event for workflow execution
func (e *historyEngineImpl) RequestCancelWorkflowExecution(
	ctx context.Context,
	req *historyservice.RequestCancelWorkflowExecutionRequest,
) (resp *historyservice.RequestCancelWorkflowExecutionResponse, retError error) {
	return requestcancelworkflow.Invoke(ctx, req, e.shard, e.workflowConsistencyChecker)
}

func (e *historyEngineImpl) SignalWorkflowExecution(
	ctx context.Context,
	req *historyservice.SignalWorkflowExecutionRequest,
) (resp *historyservice.SignalWorkflowExecutionResponse, retError error) {
	return signalworkflow.Invoke(ctx, req, e.shard, e.workflowConsistencyChecker)
}

// SignalWithStartWorkflowExecution signals current workflow (if running) or creates & signals a new workflow
// Consistency guarantee: always write
func (e *historyEngineImpl) SignalWithStartWorkflowExecution(
	ctx context.Context,
	req *historyservice.SignalWithStartWorkflowExecutionRequest,
) (_ *historyservice.SignalWithStartWorkflowExecutionResponse, retError error) {
	return signalwithstartworkflow.Invoke(ctx, req, e.shard, e.workflowConsistencyChecker)
}

func (h *historyEngineImpl) UpdateWorkflow(
	ctx context.Context,
	request *historyservice.UpdateWorkflowRequest,
) (*historyservice.UpdateWorkflowResponse, error) {
	return nil, serviceerror.NewUnimplemented("UpdateWorkflow is not supported on this server")
}

// RemoveSignalMutableState remove the signal request id in signal_requested for deduplicate
func (e *historyEngineImpl) RemoveSignalMutableState(
	ctx context.Context,
	request *historyservice.RemoveSignalMutableStateRequest,
) error {

	_, err := e.getActiveNamespaceEntry(namespace.ID(request.GetNamespaceId()))
	if err != nil {
		return err
	}

	return api.GetAndUpdateWorkflowWithNew(
		ctx,
		nil,
		api.BypassMutableStateConsistencyPredicate,
		definition.NewWorkflowKey(
			request.NamespaceId,
			request.WorkflowExecution.WorkflowId,
			request.WorkflowExecution.RunId,
		),
		func(workflowContext api.WorkflowContext) (*api.UpdateWorkflowAction, error) {
			mutableState := workflowContext.GetMutableState()
			if !mutableState.IsWorkflowExecutionRunning() {
				return nil, consts.ErrWorkflowCompleted
			}

			mutableState.DeleteSignalRequested(request.GetRequestId())
			return &api.UpdateWorkflowAction{
				Noop:               false,
				CreateWorkflowTask: false,
			}, nil
		},
		nil,
		e.shard,
		e.workflowConsistencyChecker,
	)
}

func (e *historyEngineImpl) TerminateWorkflowExecution(
	ctx context.Context,
	req *historyservice.TerminateWorkflowExecutionRequest,
) (*historyservice.TerminateWorkflowExecutionResponse, error) {
	return terminateworkflow.Invoke(ctx, req, e.shard, e.workflowConsistencyChecker)
}

func (e *historyEngineImpl) DeleteWorkflowExecution(
	ctx context.Context,
	request *historyservice.DeleteWorkflowExecutionRequest,
) (retError error) {

	weCtx, err := e.workflowConsistencyChecker.GetWorkflowContext(
		ctx,
		nil,
		api.BypassMutableStateConsistencyPredicate,
		definition.NewWorkflowKey(
			request.NamespaceId,
			request.WorkflowExecution.WorkflowId,
			request.WorkflowExecution.RunId,
		),
	)
	if err != nil {
		return err
	}
	defer func() { weCtx.GetReleaseFn()(retError) }()

	// Open and Close workflow executions are deleted differently.
	// Open workflow execution is deleted by terminating with special flag `deleteAfterTerminate` set to true.
	// This flag will be carried over with CloseExecutionTask and workflow will be deleted as the last step while processing the task.
	//
	// Close workflow execution is deleted using DeleteExecutionTask.
	//
	// DeleteWorkflowExecution is not replicated automatically. Workflow executions must be deleted separately in each cluster.
	// Although running workflows in active cluster are terminated first and the termination event might be replicated.
	// In passive cluster, workflow executions are just deleted in regardless of its state.

	if weCtx.GetMutableState().IsWorkflowExecutionRunning() {
		if request.GetClosedWorkflowOnly() {
			// skip delete open workflow
			return nil
		}
		ns, err := e.shard.GetNamespaceRegistry().GetNamespaceByID(namespace.ID(request.GetNamespaceId()))
		if err != nil {
			return err
		}
		if ns.ActiveInCluster(e.shard.GetClusterMetadata().GetCurrentClusterName()) {
			// If workflow execution is running and in active cluster.
			return api.UpdateWorkflowWithNew(
				e.shard,
				ctx,
				weCtx,
				func(workflowContext api.WorkflowContext) (*api.UpdateWorkflowAction, error) {
					mutableState := workflowContext.GetMutableState()
					eventBatchFirstEventID := mutableState.GetNextEventID()

					return api.UpdateWorkflowWithoutWorkflowTask, workflow.TerminateWorkflow(
						mutableState,
						eventBatchFirstEventID,
						"Delete workflow execution",
						nil,
						consts.IdentityHistoryService,
						true,
					)
				},
				nil,
			)
		}
	}

	// If workflow execution is closed or in passive cluster.
	return e.workflowDeleteManager.AddDeleteWorkflowExecutionTask(
		ctx,
		namespace.ID(request.GetNamespaceId()),
		commonpb.WorkflowExecution{
			WorkflowId: request.GetWorkflowExecution().GetWorkflowId(),
			RunId:      request.GetWorkflowExecution().GetRunId(),
		},
		weCtx.GetMutableState(),
		request.GetWorkflowVersion(),
	)
}

// RecordChildExecutionCompleted records the completion of child execution into parent execution history
func (e *historyEngineImpl) RecordChildExecutionCompleted(
	ctx context.Context,
	completionRequest *historyservice.RecordChildExecutionCompletedRequest,
) error {

	_, err := e.getActiveNamespaceEntry(namespace.ID(completionRequest.GetNamespaceId()))
	if err != nil {
		return err
	}

	parentInitiatedID := completionRequest.ParentInitiatedId
	parentInitiatedVersion := completionRequest.ParentInitiatedVersion

	return api.GetAndUpdateWorkflowWithNew(
		ctx,
		completionRequest.Clock,
		func(mutableState workflow.MutableState) bool {
			if !mutableState.IsWorkflowExecutionRunning() {
				// current branch already closed, we won't perform any operation, pass the check
				return true
			}

			onCurrentBranch, err := historyEventOnCurrentBranch(mutableState, parentInitiatedID, parentInitiatedVersion)
			if err != nil {
				// can't find initiated event, potential stale mutable, fail the predicate check
				return false
			}
			if !onCurrentBranch {
				// found on different branch, since we don't record completion on a different branch, pass the check
				return true
			}

			ci, isRunning := mutableState.GetChildExecutionInfo(parentInitiatedID)
			return !(isRunning && ci.StartedEventId == common.EmptyEventID) // !(potential stale)
		},
		definition.NewWorkflowKey(
			completionRequest.NamespaceId,
			completionRequest.WorkflowExecution.WorkflowId,
			completionRequest.WorkflowExecution.RunId,
		),
		func(workflowContext api.WorkflowContext) (*api.UpdateWorkflowAction, error) {
			mutableState := workflowContext.GetMutableState()
			if !mutableState.IsWorkflowExecutionRunning() {
				return nil, consts.ErrWorkflowCompleted
			}

			onCurrentBranch, err := historyEventOnCurrentBranch(mutableState, parentInitiatedID, parentInitiatedVersion)
			if err != nil || !onCurrentBranch {
				return nil, consts.ErrChildExecutionNotFound
			}

			// Check mutable state to make sure child execution is in pending child executions
			ci, isRunning := mutableState.GetChildExecutionInfo(parentInitiatedID)
			if !isRunning || ci.StartedEventId == common.EmptyEventID {
				// note we already checked if startedEventID is empty (in consistency predicate)
				// and reloaded mutable state
				return nil, consts.ErrChildExecutionNotFound
			}

			completedExecution := completionRequest.CompletedExecution
			if ci.GetStartedWorkflowId() != completedExecution.GetWorkflowId() {
				// this can only happen when we don't have the initiated version
				return nil, consts.ErrChildExecutionNotFound
			}

			completionEvent := completionRequest.CompletionEvent
			switch completionEvent.GetEventType() {
			case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED:
				attributes := completionEvent.GetWorkflowExecutionCompletedEventAttributes()
				_, err = mutableState.AddChildWorkflowExecutionCompletedEvent(parentInitiatedID, completedExecution, attributes)
			case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED:
				attributes := completionEvent.GetWorkflowExecutionFailedEventAttributes()
				_, err = mutableState.AddChildWorkflowExecutionFailedEvent(parentInitiatedID, completedExecution, attributes)
			case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCELED:
				attributes := completionEvent.GetWorkflowExecutionCanceledEventAttributes()
				_, err = mutableState.AddChildWorkflowExecutionCanceledEvent(parentInitiatedID, completedExecution, attributes)
			case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TERMINATED:
				attributes := completionEvent.GetWorkflowExecutionTerminatedEventAttributes()
				_, err = mutableState.AddChildWorkflowExecutionTerminatedEvent(parentInitiatedID, completedExecution, attributes)
			case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TIMED_OUT:
				attributes := completionEvent.GetWorkflowExecutionTimedOutEventAttributes()
				_, err = mutableState.AddChildWorkflowExecutionTimedOutEvent(parentInitiatedID, completedExecution, attributes)
			}

			if err != nil {
				return nil, err
			}
			return &api.UpdateWorkflowAction{
				Noop:               false,
				CreateWorkflowTask: true,
			}, nil
		},
		nil,
		e.shard,
		e.workflowConsistencyChecker,
	)
}

func (e *historyEngineImpl) VerifyChildExecutionCompletionRecorded(
	ctx context.Context,
	request *historyservice.VerifyChildExecutionCompletionRecordedRequest,
) (retError error) {
	namespaceID := namespace.ID(request.GetNamespaceId())
	if err := api.ValidateNamespaceUUID(namespaceID); err != nil {
		return err
	}

	workflowContext, err := e.workflowConsistencyChecker.GetWorkflowContext(
		ctx,
		request.Clock,
		// it's ok we have stale state when doing verification,
		// the logic will return WorkflowNotReady error and the caller will retry
		// this can prevent keep reloading mutable state when there's a replication lag
		// in parent shard.
		api.BypassMutableStateConsistencyPredicate,
		definition.NewWorkflowKey(
			request.NamespaceId,
			request.ParentExecution.WorkflowId,
			request.ParentExecution.RunId,
		),
	)
	if err != nil {
		return err
	}
	defer func() { workflowContext.GetReleaseFn()(retError) }()

	mutableState := workflowContext.GetMutableState()
	if !mutableState.IsWorkflowExecutionRunning() &&
		mutableState.GetExecutionState().State != enumsspb.WORKFLOW_EXECUTION_STATE_ZOMBIE {
		// parent has already completed and can't be blocked after failover.
		return nil
	}

	onCurrentBranch, err := historyEventOnCurrentBranch(mutableState, request.ParentInitiatedId, request.ParentInitiatedVersion)
	if err != nil {
		// initiated event not found on any branch
		return consts.ErrWorkflowNotReady
	}

	if !onCurrentBranch {
		// due to conflict resolution, the initiated event may on a different branch of the workflow.
		// we don't have to do anything and can simply return not found error. Standby logic
		// after seeing this error will give up verification.
		return consts.ErrChildExecutionNotFound
	}

	ci, isRunning := mutableState.GetChildExecutionInfo(request.ParentInitiatedId)
	if isRunning {
		if ci.StartedEventId != common.EmptyEventID &&
			ci.GetStartedWorkflowId() != request.ChildExecution.GetWorkflowId() {
			// this can happen since we may not have the initiated version
			return consts.ErrChildExecutionNotFound
		}

		return consts.ErrWorkflowNotReady
	}

	return nil
}

func (e *historyEngineImpl) ReplicateEventsV2(
	ctx context.Context,
	replicateRequest *historyservice.ReplicateEventsV2Request,
) error {

	return e.nDCReplicator.ApplyEvents(ctx, replicateRequest)
}

// ReplicateWorkflowState is an experimental method to replicate workflow state. This should not expose outside of history service role.
func (e *historyEngineImpl) ReplicateWorkflowState(
	ctx context.Context,
	request *historyservice.ReplicateWorkflowStateRequest,
) error {

	return e.nDCReplicator.ApplyWorkflowState(ctx, request)
}

func (e *historyEngineImpl) SyncShardStatus(
	ctx context.Context,
	request *historyservice.SyncShardStatusRequest,
) error {

	clusterName := request.GetSourceCluster()
	now := timestamp.TimeValue(request.GetStatusTime())

	// here there are 3 main things
	// 1. update the view of remote cluster's shard time
	// 2. notify the timer gate in the timer queue standby processor
	// 3, notify the transfer (essentially a no op, just put it here so it looks symmetric)
	e.shard.SetCurrentTime(clusterName, now)
	for _, processor := range e.queueProcessors {
		processor.NotifyNewTasks(clusterName, []tasks.Task{})
	}
	return nil
}

func (e *historyEngineImpl) SyncActivity(
	ctx context.Context,
	request *historyservice.SyncActivityRequest,
) (retError error) {

	return e.nDCActivityReplicator.SyncActivity(ctx, request)
}

// ResetWorkflowExecution terminates current workflow (if running) and replay & create new workflow
// Consistency guarantee: always write
func (e *historyEngineImpl) ResetWorkflowExecution(
	ctx context.Context,
	resetRequest *historyservice.ResetWorkflowExecutionRequest,
) (response *historyservice.ResetWorkflowExecutionResponse, retError error) {

	request := resetRequest.ResetRequest
	namespaceID := namespace.ID(resetRequest.GetNamespaceId())
	workflowID := request.WorkflowExecution.GetWorkflowId()
	baseRunID := request.WorkflowExecution.GetRunId()

	baseWFContext, err := e.workflowConsistencyChecker.GetWorkflowContext(
		ctx,
		nil,
		api.BypassMutableStateConsistencyPredicate,
		definition.NewWorkflowKey(
			namespaceID.String(),
			workflowID,
			baseRunID,
		),
	)
	if err != nil {
		return nil, err
	}
	defer func() { baseWFContext.GetReleaseFn()(retError) }()

	baseMutableState := baseWFContext.GetMutableState()
	if request.GetWorkflowTaskFinishEventId() <= common.FirstEventID ||
		request.GetWorkflowTaskFinishEventId() >= baseMutableState.GetNextEventID() {
		return nil, serviceerror.NewInvalidArgument("Workflow task finish ID must be > 1 && <= workflow last event ID.")
	}

	// also load the current run of the workflow, it can be different from the base runID
	currentRunID, err := e.workflowConsistencyChecker.GetCurrentRunID(
		ctx,
		namespaceID.String(),
		request.WorkflowExecution.GetWorkflowId(),
	)
	if err != nil {
		return nil, err
	}
	if baseRunID == "" {
		baseRunID = currentRunID
	}

	var currentWFContext api.WorkflowContext
	if currentRunID == baseRunID {
		currentWFContext = baseWFContext
	} else {
		currentWFContext, err = e.workflowConsistencyChecker.GetWorkflowContext(
			ctx,
			nil,
			api.BypassMutableStateConsistencyPredicate,
			definition.NewWorkflowKey(
				namespaceID.String(),
				workflowID,
				currentRunID,
			),
		)
		if err != nil {
			return nil, err
		}
		defer func() { currentWFContext.GetReleaseFn()(retError) }()
	}

	// dedup by requestID
	if currentWFContext.GetMutableState().GetExecutionState().CreateRequestId == request.GetRequestId() {
		e.logger.Info("Duplicated reset request",
			tag.WorkflowID(workflowID),
			tag.WorkflowRunID(currentRunID),
			tag.WorkflowNamespaceID(namespaceID.String()))
		return &historyservice.ResetWorkflowExecutionResponse{
			RunId: currentRunID,
		}, nil
	}

	resetRunID := uuid.New()
	baseRebuildLastEventID := request.GetWorkflowTaskFinishEventId() - 1
	baseVersionHistories := baseMutableState.GetExecutionInfo().GetVersionHistories()
	baseCurrentVersionHistory, err := versionhistory.GetCurrentVersionHistory(baseVersionHistories)
	if err != nil {
		return nil, err
	}
	baseRebuildLastEventVersion, err := versionhistory.GetVersionHistoryEventVersion(baseCurrentVersionHistory, baseRebuildLastEventID)
	if err != nil {
		return nil, err
	}
	baseCurrentBranchToken := baseCurrentVersionHistory.GetBranchToken()
	baseNextEventID := baseMutableState.GetNextEventID()

	if err := e.workflowResetter.resetWorkflow(
		ctx,
		namespaceID,
		workflowID,
		baseRunID,
		baseCurrentBranchToken,
		baseRebuildLastEventID,
		baseRebuildLastEventVersion,
		baseNextEventID,
		resetRunID,
		request.GetRequestId(),
		newNDCWorkflow(
			ctx,
			e.shard.GetNamespaceRegistry(),
			e.shard.GetClusterMetadata(),
			currentWFContext.GetContext(),
			currentWFContext.GetMutableState(),
			currentWFContext.GetReleaseFn(),
		),
		request.GetReason(),
		nil,
		request.GetResetReapplyType(),
	); err != nil {
		return nil, err
	}
	return &historyservice.ResetWorkflowExecutionResponse{
		RunId: resetRunID,
	}, nil
}

func (e *historyEngineImpl) NotifyNewHistoryEvent(
	notification *events.Notification,
) {

	e.eventNotifier.NotifyNewHistoryEvent(notification)
}

func (e *historyEngineImpl) NotifyNewTasks(
	clusterName string,
	newTasks map[tasks.Category][]tasks.Task,
) {
	for category, tasksByCategory := range newTasks {
		// TODO: make replicatorProcessor part of queueProcessors list
		// and get rid of the special case here.
		if category == tasks.CategoryReplication {
			if e.replicationAckMgr != nil {
				e.replicationAckMgr.NotifyNewTasks(tasksByCategory)
			}
			continue
		}

		if len(tasksByCategory) > 0 {
			e.queueProcessors[category].NotifyNewTasks(clusterName, tasksByCategory)
		}
	}
}

func (e *historyEngineImpl) getActiveNamespaceEntry(
	namespaceUUID namespace.ID,
) (*namespace.Namespace, error) {
	return api.GetActiveNamespace(e.shard, namespaceUUID)
}

func (e *historyEngineImpl) GetReplicationMessages(
	ctx context.Context,
	pollingCluster string,
	ackMessageID int64,
	ackTimestamp time.Time,
	queryMessageID int64,
) (*replicationspb.ReplicationMessages, error) {

	if ackMessageID != persistence.EmptyQueueMessageID {
		if err := e.shard.UpdateQueueClusterAckLevel(
			tasks.CategoryReplication,
			pollingCluster,
			tasks.NewImmediateKey(ackMessageID),
		); err != nil {
			e.logger.Error("error updating replication level for shard", tag.Error(err), tag.OperationFailed)
		}
		e.shard.UpdateRemoteClusterInfo(pollingCluster, ackMessageID, ackTimestamp)
	}

	replicationMessages, err := e.replicationAckMgr.GetTasks(
		ctx,
		pollingCluster,
		queryMessageID,
	)
	if err != nil {
		e.logger.Error("Failed to retrieve replication messages.", tag.Error(err))
		return nil, err
	}
	e.logger.Debug("Successfully fetched replication messages.", tag.Counter(len(replicationMessages.ReplicationTasks)))

	return replicationMessages, nil
}

func (e *historyEngineImpl) GetDLQReplicationMessages(
	ctx context.Context,
	taskInfos []*replicationspb.ReplicationTaskInfo,
) ([]*replicationspb.ReplicationTask, error) {

	tasks := make([]*replicationspb.ReplicationTask, 0, len(taskInfos))
	for _, taskInfo := range taskInfos {
		task, err := e.replicationAckMgr.GetTask(ctx, taskInfo)
		if err != nil {
			e.logger.Error("Failed to fetch DLQ replication messages.", tag.Error(err))
			return nil, err
		}
		tasks = append(tasks, task)
	}

	return tasks, nil
}

func (e *historyEngineImpl) ReapplyEvents(
	ctx context.Context,
	namespaceUUID namespace.ID,
	workflowID string,
	runID string,
	reapplyEvents []*historypb.HistoryEvent,
) error {

	if e.config.SkipReapplicationByNamespaceID(namespaceUUID.String()) {
		return nil
	}

	namespaceEntry, err := e.getActiveNamespaceEntry(namespaceUUID)
	if err != nil {
		return err
	}
	namespaceID := namespaceEntry.ID()

	return api.GetAndUpdateWorkflowWithNew(
		ctx,
		nil,
		api.BypassMutableStateConsistencyPredicate,
		definition.NewWorkflowKey(
			namespaceID.String(),
			workflowID,
			"",
		),
		func(workflowContext api.WorkflowContext) (action *api.UpdateWorkflowAction, retErr error) {
			context := workflowContext.GetContext()
			mutableState := workflowContext.GetMutableState()
			// Filter out reapply event from the same cluster
			toReapplyEvents := make([]*historypb.HistoryEvent, 0, len(reapplyEvents))
			lastWriteVersion, err := mutableState.GetLastWriteVersion()
			if err != nil {
				return nil, err
			}
			sourceMutableState := mutableState
			if sourceMutableState.GetWorkflowKey().RunID != runID {
				originCtx, err := e.workflowConsistencyChecker.GetWorkflowContext(
					ctx,
					nil,
					api.BypassMutableStateConsistencyPredicate,
					definition.NewWorkflowKey(namespaceID.String(), workflowID, runID),
				)
				if err != nil {
					return nil, err
				}
				defer func() { originCtx.GetReleaseFn()(retErr) }()
				sourceMutableState = originCtx.GetMutableState()
			}

			for _, event := range reapplyEvents {
				if event.GetVersion() == lastWriteVersion {
					// The reapply is from the same cluster. Ignoring.
					continue
				}
				dedupResource := definition.NewEventReappliedID(runID, event.GetEventId(), event.GetVersion())
				if mutableState.IsResourceDuplicated(dedupResource) {
					// already apply the signal
					continue
				}
				versionHistories := sourceMutableState.GetExecutionInfo().GetVersionHistories()
				if e.containsHistoryEvent(versionHistories, event.GetEventId(), event.GetVersion()) {
					continue
				}

				toReapplyEvents = append(toReapplyEvents, event)
			}
			if len(toReapplyEvents) == 0 {
				return &api.UpdateWorkflowAction{
					Noop:               true,
					CreateWorkflowTask: false,
				}, nil
			}

			if !mutableState.IsWorkflowExecutionRunning() {
				// need to reset target workflow (which is also the current workflow)
				// to accept events to be reapplied
				baseRunID := mutableState.GetExecutionState().GetRunId()
				resetRunID := uuid.New()
				baseRebuildLastEventID := mutableState.GetPreviousStartedEventID()

				// TODO when https://github.com/uber/cadence/issues/2420 is finished, remove this block,
				//  since cannot reapply event to a finished workflow which had no workflow tasks started
				if baseRebuildLastEventID == common.EmptyEventID {
					e.logger.Warn("cannot reapply event to a finished workflow with no workflow task",
						tag.WorkflowNamespaceID(namespaceID.String()),
						tag.WorkflowID(workflowID),
					)
					e.metricsClient.IncCounter(metrics.HistoryReapplyEventsScope, metrics.EventReapplySkippedCount)
					return &api.UpdateWorkflowAction{
						Noop:               true,
						CreateWorkflowTask: false,
					}, nil
				}

				baseVersionHistories := mutableState.GetExecutionInfo().GetVersionHistories()
				baseCurrentVersionHistory, err := versionhistory.GetCurrentVersionHistory(baseVersionHistories)
				if err != nil {
					return nil, err
				}
				baseRebuildLastEventVersion, err := versionhistory.GetVersionHistoryEventVersion(baseCurrentVersionHistory, baseRebuildLastEventID)
				if err != nil {
					return nil, err
				}
				baseCurrentBranchToken := baseCurrentVersionHistory.GetBranchToken()
				baseNextEventID := mutableState.GetNextEventID()

				err = e.workflowResetter.resetWorkflow(
					ctx,
					namespaceID,
					workflowID,
					baseRunID,
					baseCurrentBranchToken,
					baseRebuildLastEventID,
					baseRebuildLastEventVersion,
					baseNextEventID,
					resetRunID,
					uuid.New(),
					newNDCWorkflow(
						ctx,
						e.shard.GetNamespaceRegistry(),
						e.shard.GetClusterMetadata(),
						context,
						mutableState,
						workflow.NoopReleaseFn,
					),
					eventsReapplicationResetWorkflowReason,
					toReapplyEvents,
					enumspb.RESET_REAPPLY_TYPE_SIGNAL,
				)
				switch err.(type) {
				case *serviceerror.InvalidArgument:
					// no-op. Usually this is due to reset workflow with pending child workflows
					e.logger.Warn("Cannot reset workflow. Ignoring reapply events.", tag.Error(err))
				case nil:
					// no-op
				default:
					return nil, err
				}
				return &api.UpdateWorkflowAction{
					Noop:               true,
					CreateWorkflowTask: false,
				}, nil
			}

			postActions := &api.UpdateWorkflowAction{
				Noop:               false,
				CreateWorkflowTask: true,
			}
			if mutableState.IsWorkflowPendingOnWorkflowTaskBackoff() {
				// Do not create workflow task when the workflow has first workflow task backoff and execution is not started yet
				postActions.CreateWorkflowTask = false
			}
			reappliedEvents, err := e.eventsReapplier.reapplyEvents(
				ctx,
				mutableState,
				toReapplyEvents,
				runID,
			)
			if err != nil {
				e.logger.Error("failed to re-apply stale events", tag.Error(err))
				return nil, err
			}
			if len(reappliedEvents) == 0 {
				return &api.UpdateWorkflowAction{
					Noop:               true,
					CreateWorkflowTask: false,
				}, nil
			}
			return postActions, nil
		},
		nil,
		e.shard,
		e.workflowConsistencyChecker,
	)
}

func (e *historyEngineImpl) GetDLQMessages(
	ctx context.Context,
	request *historyservice.GetDLQMessagesRequest,
) (*historyservice.GetDLQMessagesResponse, error) {

	_, ok := e.clusterMetadata.GetAllClusterInfo()[request.GetSourceCluster()]
	if !ok {
		return nil, consts.ErrUnknownCluster
	}

	tasks, token, err := e.replicationDLQHandler.GetMessages(
		ctx,
		request.GetSourceCluster(),
		request.GetInclusiveEndMessageId(),
		int(request.GetMaximumPageSize()),
		request.GetNextPageToken(),
	)
	if err != nil {
		return nil, err
	}
	return &historyservice.GetDLQMessagesResponse{
		Type:             request.GetType(),
		ReplicationTasks: tasks,
		NextPageToken:    token,
	}, nil
}

func (e *historyEngineImpl) PurgeDLQMessages(
	ctx context.Context,
	request *historyservice.PurgeDLQMessagesRequest,
) error {

	_, ok := e.clusterMetadata.GetAllClusterInfo()[request.GetSourceCluster()]
	if !ok {
		return consts.ErrUnknownCluster
	}

	return e.replicationDLQHandler.PurgeMessages(
		ctx,
		request.GetSourceCluster(),
		request.GetInclusiveEndMessageId(),
	)
}

func (e *historyEngineImpl) MergeDLQMessages(
	ctx context.Context,
	request *historyservice.MergeDLQMessagesRequest,
) (*historyservice.MergeDLQMessagesResponse, error) {

	_, ok := e.clusterMetadata.GetAllClusterInfo()[request.GetSourceCluster()]
	if !ok {
		return nil, consts.ErrUnknownCluster
	}

	token, err := e.replicationDLQHandler.MergeMessages(
		ctx,
		request.GetSourceCluster(),
		request.GetInclusiveEndMessageId(),
		int(request.GetMaximumPageSize()),
		request.GetNextPageToken(),
	)
	if err != nil {
		return nil, err
	}
	return &historyservice.MergeDLQMessagesResponse{
		NextPageToken: token,
	}, nil
}

func (e *historyEngineImpl) RebuildMutableState(
	ctx context.Context,
	namespaceUUID namespace.ID,
	execution commonpb.WorkflowExecution,
) error {
	return e.workflowRebuilder.rebuild(
		ctx,
		definition.NewWorkflowKey(
			namespaceUUID.String(),
			execution.GetWorkflowId(),
			execution.GetRunId(),
		),
	)
}

func (e *historyEngineImpl) RefreshWorkflowTasks(
	ctx context.Context,
	namespaceUUID namespace.ID,
	execution commonpb.WorkflowExecution,
) (retError error) {

	err := api.ValidateNamespaceUUID(namespaceUUID)
	if err != nil {
		return err
	}

	wfContext, err := e.workflowConsistencyChecker.GetWorkflowContext(
		ctx,
		nil,
		api.BypassMutableStateConsistencyPredicate,
		definition.NewWorkflowKey(
			namespaceUUID.String(),
			execution.WorkflowId,
			execution.RunId,
		),
	)
	if err != nil {
		return err
	}
	defer func() { wfContext.GetReleaseFn()(retError) }()

	mutableState := wfContext.GetMutableState()
	mutableStateTaskRefresher := workflow.NewTaskRefresher(
		e.shard,
		e.shard.GetConfig(),
		e.shard.GetNamespaceRegistry(),
		e.shard.GetEventsCache(),
		e.shard.GetLogger(),
	)

	now := e.shard.GetTimeSource().Now()

	err = mutableStateTaskRefresher.RefreshTasks(ctx, now, mutableState)
	if err != nil {
		return err
	}

	return e.shard.AddTasks(ctx, &persistence.AddHistoryTasksRequest{
		ShardID: e.shard.GetShardID(),
		// RangeID is set by shard
		NamespaceID: namespaceUUID.String(),
		WorkflowID:  execution.WorkflowId,
		RunID:       execution.RunId,
		Tasks:       mutableState.PopTasks(),
	})
}

func (e *historyEngineImpl) GenerateLastHistoryReplicationTasks(
	ctx context.Context,
	request *historyservice.GenerateLastHistoryReplicationTasksRequest,
) (_ *historyservice.GenerateLastHistoryReplicationTasksResponse, retError error) {
	namespaceEntry, err := e.getActiveNamespaceEntry(namespace.ID(request.GetNamespaceId()))
	if err != nil {
		return nil, err
	}
	namespaceID := namespaceEntry.ID()

	wfContext, err := e.workflowConsistencyChecker.GetWorkflowContext(
		ctx,
		nil,
		api.BypassMutableStateConsistencyPredicate,
		definition.NewWorkflowKey(
			namespaceID.String(),
			request.Execution.WorkflowId,
			request.Execution.RunId,
		),
	)
	if err != nil {
		return nil, err
	}
	defer func() { wfContext.GetReleaseFn()(retError) }()

	now := e.shard.GetTimeSource().Now()
	task, err := wfContext.GetMutableState().GenerateMigrationTasks(now)
	if err != nil {
		return nil, err
	}

	err = e.shard.AddTasks(ctx, &persistence.AddHistoryTasksRequest{
		ShardID: e.shard.GetShardID(),
		// RangeID is set by shard
		NamespaceID: string(namespaceID),
		WorkflowID:  request.Execution.WorkflowId,
		RunID:       request.Execution.RunId,
		Tasks: map[tasks.Category][]tasks.Task{
			tasks.CategoryReplication: {task},
		},
	})
	if err != nil {
		return nil, err
	}
	return &historyservice.GenerateLastHistoryReplicationTasksResponse{}, nil
}

func (e *historyEngineImpl) GetReplicationStatus(
	ctx context.Context,
	request *historyservice.GetReplicationStatusRequest,
) (_ *historyservice.ShardReplicationStatus, retError error) {

	resp := &historyservice.ShardReplicationStatus{
		ShardId:        e.shard.GetShardID(),
		ShardLocalTime: timestamp.TimePtr(e.shard.GetTimeSource().Now()),
	}

	maxReplicationTaskId, maxTaskVisibilityTimeStamp := e.replicationAckMgr.GetMaxTaskInfo()
	resp.MaxReplicationTaskId = maxReplicationTaskId
	resp.MaxReplicationTaskVisibilityTime = timestamp.TimePtr(maxTaskVisibilityTimeStamp)

	remoteClusters, handoverNamespaces, err := e.shard.GetReplicationStatus(request.RemoteClusters)
	if err != nil {
		return nil, err
	}
	resp.RemoteClusters = remoteClusters
	resp.HandoverNamespaces = handoverNamespaces
	return resp, nil
}

func (e *historyEngineImpl) containsHistoryEvent(
	versionHistories *historyspb.VersionHistories,
	reappliedEventID int64,
	reappliedEventVersion int64,
) bool {
	// Check if the source workflow contains the reapply event.
	// If it does, it means the event is received in this cluster, no need to reapply.
	_, err := versionhistory.FindFirstVersionHistoryIndexByVersionHistoryItem(
		versionHistories,
		versionhistory.NewVersionHistoryItem(reappliedEventID, reappliedEventVersion),
	)
	return err == nil
}

func historyEventOnCurrentBranch(
	mutableState workflow.MutableState,
	eventID int64,
	eventVersion int64,
) (bool, error) {
	if eventVersion == 0 {
		if eventID >= mutableState.GetNextEventID() {
			return false, &serviceerror.NotFound{Message: "History event not found"}
		}

		// there's no version, assume the event is on the current branch
		return true, nil
	}

	versionHistoryies := mutableState.GetExecutionInfo().GetVersionHistories()
	versionHistoryItem := versionhistory.NewVersionHistoryItem(eventID, eventVersion)
	if _, err := versionhistory.FindFirstVersionHistoryIndexByVersionHistoryItem(
		versionHistoryies,
		versionHistoryItem,
	); err != nil {
		return false, &serviceerror.NotFound{Message: "History event not found"}
	}

	// check if on current branch
	currentVersionHistory, err := versionhistory.GetCurrentVersionHistory(versionHistoryies)
	if err != nil {
		return false, err
	}

	return versionhistory.ContainsVersionHistoryItem(
		currentVersionHistory,
		versionHistoryItem,
	), nil
}
