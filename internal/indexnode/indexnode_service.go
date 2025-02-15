// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package indexnode

import (
	"context"
	"fmt"
	"strconv"

	"github.com/golang/protobuf/proto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus/internal/proto/indexpb"
	"github.com/milvus-io/milvus/pkg/common"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/metrics"
	"github.com/milvus-io/milvus/pkg/util/commonpbutil"
	"github.com/milvus-io/milvus/pkg/util/merr"
	"github.com/milvus-io/milvus/pkg/util/metricsinfo"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
	"github.com/milvus-io/milvus/pkg/util/timerecord"
	"github.com/milvus-io/milvus/pkg/util/typeutil"
)

func (i *IndexNode) CreateJob(ctx context.Context, req *indexpb.CreateJobRequest) (*commonpb.Status, error) {
	if !i.lifetime.Add(commonpbutil.IsHealthy) {
		stateCode := i.lifetime.GetState()
		log.Ctx(ctx).Warn("index node not ready",
			zap.String("state", stateCode.String()),
			zap.String("clusterID", req.GetClusterID()),
			zap.Int64("indexBuildID", req.GetBuildID()),
		)
		return merr.Status(merr.WrapErrServiceNotReady(stateCode.String())), nil
	}
	defer i.lifetime.Done()
	log.Ctx(ctx).Info("IndexNode building index ...",
		zap.String("clusterID", req.GetClusterID()),
		zap.Int64("indexBuildID", req.GetBuildID()),
		zap.Int64("indexID", req.GetIndexID()),
		zap.String("indexName", req.GetIndexName()),
		zap.String("indexFilePrefix", req.GetIndexFilePrefix()),
		zap.Int64("indexVersion", req.GetIndexVersion()),
		zap.Strings("dataPaths", req.GetDataPaths()),
		zap.Any("typeParams", req.GetTypeParams()),
		zap.Any("indexParams", req.GetIndexParams()),
		zap.Int64("numRows", req.GetNumRows()),
	)
	ctx, sp := otel.Tracer(typeutil.IndexNodeRole).Start(ctx, "IndexNode-CreateIndex", trace.WithAttributes(
		attribute.Int64("indexBuildID", req.GetBuildID()),
		attribute.String("clusterID", req.GetClusterID()),
	))
	defer sp.End()
	metrics.IndexNodeBuildIndexTaskCounter.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10), metrics.TotalLabel).Inc()

	taskCtx, taskCancel := context.WithCancel(i.loopCtx)
	if oldInfo := i.loadOrStoreTask(req.GetClusterID(), req.GetBuildID(), &taskInfo{
		cancel: taskCancel,
		state:  commonpb.IndexState_InProgress,
	}); oldInfo != nil {
		log.Ctx(ctx).Warn("duplicated index build task", zap.String("clusterID", req.GetClusterID()), zap.Int64("buildID", req.GetBuildID()))
		metrics.IndexNodeBuildIndexTaskCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.FailLabel).Inc()
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_BuildIndexError,
			Reason:    "duplicated index build task",
		}, nil
	}
	cm, err := i.storageFactory.NewChunkManager(i.loopCtx, req.GetStorageConfig())
	if err != nil {
		log.Ctx(ctx).Error("create chunk manager failed", zap.String("bucket", req.GetStorageConfig().GetBucketName()),
			zap.String("accessKey", req.GetStorageConfig().GetAccessKeyID()),
			zap.String("clusterID", req.GetClusterID()), zap.Int64("indexBuildID", req.GetBuildID()),
			zap.Error(err),
		)
		i.deleteTaskInfos(ctx, []taskKey{{ClusterID: req.GetClusterID(), BuildID: req.GetBuildID()}})
		metrics.IndexNodeBuildIndexTaskCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.FailLabel).Inc()
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_BuildIndexError,
			Reason:    "create chunk manager failed, error: " + err.Error(),
		}, nil
	}
	task := &indexBuildTask{
		ident:          fmt.Sprintf("%s/%d", req.ClusterID, req.BuildID),
		ctx:            taskCtx,
		cancel:         taskCancel,
		BuildID:        req.GetBuildID(),
		ClusterID:      req.GetClusterID(),
		node:           i,
		req:            req,
		cm:             cm,
		nodeID:         i.GetNodeID(),
		tr:             timerecord.NewTimeRecorder(fmt.Sprintf("IndexBuildID: %d, ClusterID: %s", req.BuildID, req.ClusterID)),
		serializedSize: 0,
	}
	ret := merr.Status(nil)
	if err := i.sched.IndexBuildQueue.Enqueue(task); err != nil {
		log.Ctx(ctx).Warn("IndexNode failed to schedule", zap.Int64("indexBuildID", req.GetBuildID()),
			zap.String("clusterID", req.GetClusterID()), zap.Error(err))
		ret.ErrorCode = commonpb.ErrorCode_UnexpectedError
		ret.Reason = err.Error()
		metrics.IndexNodeBuildIndexTaskCounter.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10), metrics.FailLabel).Inc()
		return ret, nil
	}
	metrics.IndexNodeBuildIndexTaskCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), metrics.SuccessLabel).Inc()
	log.Ctx(ctx).Info("IndexNode successfully scheduled", zap.Int64("indexBuildID", req.GetBuildID()),
		zap.String("clusterID", req.GetClusterID()), zap.String("indexName", req.GetIndexName()))
	return ret, nil
}

func (i *IndexNode) QueryJobs(ctx context.Context, req *indexpb.QueryJobsRequest) (*indexpb.QueryJobsResponse, error) {
	log := log.Ctx(ctx).With(
		zap.String("clusterID", req.GetClusterID()),
	).WithRateGroup("in.queryJobs", 1, 60)
	if !i.lifetime.Add(commonpbutil.IsHealthyOrStopping) {
		stateCode := i.lifetime.GetState()
		log.Warn("index node not ready", zap.String("state", stateCode.String()))
		return &indexpb.QueryJobsResponse{
			Status: merr.Status(merr.WrapErrServiceNotReady(stateCode.String())),
		}, nil
	}
	defer i.lifetime.Done()
	infos := make(map[UniqueID]*taskInfo)
	i.foreachTaskInfo(func(ClusterID string, buildID UniqueID, info *taskInfo) {
		if ClusterID == req.GetClusterID() {
			infos[buildID] = &taskInfo{
				state:          info.state,
				fileKeys:       common.CloneStringList(info.fileKeys),
				serializedSize: info.serializedSize,
				failReason:     info.failReason,
			}
		}
	})
	ret := &indexpb.QueryJobsResponse{
		Status:     merr.Status(nil),
		ClusterID:  req.GetClusterID(),
		IndexInfos: make([]*indexpb.IndexTaskInfo, 0, len(req.GetBuildIDs())),
	}
	for i, buildID := range req.GetBuildIDs() {
		ret.IndexInfos = append(ret.IndexInfos, &indexpb.IndexTaskInfo{
			BuildID:        buildID,
			State:          commonpb.IndexState_IndexStateNone,
			IndexFileKeys:  nil,
			SerializedSize: 0,
		})
		if info, ok := infos[buildID]; ok {
			ret.IndexInfos[i].State = info.state
			ret.IndexInfos[i].IndexFileKeys = info.fileKeys
			ret.IndexInfos[i].SerializedSize = info.serializedSize
			ret.IndexInfos[i].FailReason = info.failReason
			log.RatedDebug(5, "querying index build task",
				zap.Int64("indexBuildID", buildID),
				zap.String("state", info.state.String()),
				zap.String("reason", info.failReason),
			)
		}
	}
	return ret, nil
}

func (i *IndexNode) DropJobs(ctx context.Context, req *indexpb.DropJobsRequest) (*commonpb.Status, error) {
	log.Ctx(ctx).Info("drop index build jobs",
		zap.String("clusterID", req.ClusterID),
		zap.Int64s("indexBuildIDs", req.BuildIDs),
	)
	if !i.lifetime.Add(commonpbutil.IsHealthyOrStopping) {
		stateCode := i.lifetime.GetState()
		log.Ctx(ctx).Warn("index node not ready", zap.String("state", stateCode.String()), zap.String("clusterID", req.ClusterID))
		return merr.Status(merr.WrapErrServiceNotReady(stateCode.String())), nil
	}
	defer i.lifetime.Done()
	keys := make([]taskKey, 0, len(req.GetBuildIDs()))
	for _, buildID := range req.GetBuildIDs() {
		keys = append(keys, taskKey{ClusterID: req.GetClusterID(), BuildID: buildID})
	}
	infos := i.deleteTaskInfos(ctx, keys)
	for _, info := range infos {
		if info.cancel != nil {
			info.cancel()
		}
	}
	log.Ctx(ctx).Info("drop index build jobs success", zap.String("clusterID", req.GetClusterID()),
		zap.Int64s("indexBuildIDs", req.GetBuildIDs()))
	return merr.Status(nil), nil
}

func (i *IndexNode) GetJobStats(ctx context.Context, req *indexpb.GetJobStatsRequest) (*indexpb.GetJobStatsResponse, error) {
	if !i.lifetime.Add(commonpbutil.IsHealthyOrStopping) {
		stateCode := i.lifetime.GetState()
		log.Ctx(ctx).Warn("index node not ready", zap.String("state", stateCode.String()))
		return &indexpb.GetJobStatsResponse{
			Status: merr.Status(merr.WrapErrServiceNotReady(stateCode.String())),
		}, nil
	}
	defer i.lifetime.Done()
	unissued, active := i.sched.IndexBuildQueue.GetTaskNum()
	jobInfos := make([]*indexpb.JobInfo, 0)
	i.foreachTaskInfo(func(ClusterID string, buildID UniqueID, info *taskInfo) {
		if info.statistic != nil {
			jobInfos = append(jobInfos, proto.Clone(info.statistic).(*indexpb.JobInfo))
		}
	})
	slots := 0
	if i.sched.buildParallel > unissued+active {
		slots = i.sched.buildParallel - unissued - active
	}
	log.Ctx(ctx).Info("Get Index Job Stats",
		zap.Int("unissued", unissued),
		zap.Int("active", active),
		zap.Int("slot", slots),
	)
	return &indexpb.GetJobStatsResponse{
		Status:           merr.Status(nil),
		TotalJobNum:      int64(active) + int64(unissued),
		InProgressJobNum: int64(active),
		EnqueueJobNum:    int64(unissued),
		TaskSlots:        int64(slots),
		JobInfos:         jobInfos,
		EnableDisk:       Params.IndexNodeCfg.EnableDisk.GetAsBool(),
	}, nil
}

// GetMetrics gets the metrics info of IndexNode.
// TODO(dragondriver): cache the Metrics and set a retention to the cache
func (i *IndexNode) GetMetrics(ctx context.Context, req *milvuspb.GetMetricsRequest) (*milvuspb.GetMetricsResponse, error) {
	if !i.lifetime.Add(commonpbutil.IsHealthyOrStopping) {
		log.Ctx(ctx).Warn("IndexNode.GetMetrics failed",
			zap.Int64("nodeID", paramtable.GetNodeID()),
			zap.String("req", req.GetRequest()),
			zap.Error(errIndexNodeIsUnhealthy(paramtable.GetNodeID())))

		return &milvuspb.GetMetricsResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    msgIndexNodeIsUnhealthy(paramtable.GetNodeID()),
			},
			Response: "",
		}, nil
	}
	defer i.lifetime.Done()

	metricType, err := metricsinfo.ParseMetricType(req.GetRequest())
	if err != nil {
		log.Ctx(ctx).Warn("IndexNode.GetMetrics failed to parse metric type",
			zap.Int64("nodeID", paramtable.GetNodeID()),
			zap.String("req", req.GetRequest()),
			zap.Error(err))

		return &milvuspb.GetMetricsResponse{
			Status:   merr.Status(err),
			Response: "",
		}, nil
	}

	if metricType == metricsinfo.SystemInfoMetrics {
		metrics, err := getSystemInfoMetrics(ctx, req, i)

		log.Ctx(ctx).RatedDebug(60, "IndexNode.GetMetrics",
			zap.Int64("nodeID", paramtable.GetNodeID()),
			zap.String("req", req.GetRequest()),
			zap.String("metricType", metricType),
			zap.Error(err))

		return metrics, nil
	}

	log.Ctx(ctx).RatedWarn(60, "IndexNode.GetMetrics failed, request metric type is not implemented yet",
		zap.Int64("nodeID", paramtable.GetNodeID()),
		zap.String("req", req.GetRequest()),
		zap.String("metricType", metricType))

	return &milvuspb.GetMetricsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    metricsinfo.MsgUnimplementedMetric,
		},
		Response: "",
	}, nil
}
