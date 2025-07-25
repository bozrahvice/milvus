package proxy

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/samber/lo"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/parser/planparserv2"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/exprutil"
	"github.com/milvus-io/milvus/internal/util/function"
	"github.com/milvus-io/milvus/internal/util/function/rerank"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/metrics"
	"github.com/milvus-io/milvus/pkg/v2/proto/internalpb"
	"github.com/milvus-io/milvus/pkg/v2/proto/planpb"
	"github.com/milvus-io/milvus/pkg/v2/proto/querypb"
	"github.com/milvus-io/milvus/pkg/v2/util/commonpbutil"
	"github.com/milvus-io/milvus/pkg/v2/util/funcutil"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/metric"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/timerecord"
	"github.com/milvus-io/milvus/pkg/v2/util/tsoutil"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

const (
	SearchTaskName = "SearchTask"
	SearchLevelKey = "level"

	// requeryThreshold is the estimated threshold for the size of the search results.
	// If the number of estimated search results exceeds this threshold,
	// a second query request will be initiated to retrieve output fields data.
	// In this case, the first search will not return any output field from QueryNodes.
	requeryThreshold = 0.5 * 1024 * 1024
	radiusKey        = "radius"
	rangeFilterKey   = "range_filter"
)

// type requery func(span trace.Span, ids *schemapb.IDs, outputFields []string) (*milvuspb.QueryResults, error)

type searchTask struct {
	baseTask
	Condition
	ctx context.Context
	*internalpb.SearchRequest

	result  *milvuspb.SearchResults
	request *milvuspb.SearchRequest

	tr                     *timerecord.TimeRecorder
	collectionName         string
	schema                 *schemaInfo
	needRequery            bool
	partitionKeyMode       bool
	enableMaterializedView bool
	mustUsePartitionKey    bool
	resultSizeInsufficient bool
	isTopkReduce           bool
	isRecallEvaluation     bool

	translatedOutputFields []string
	userOutputFields       []string
	userDynamicFields      []string

	resultBuf *typeutil.ConcurrentSet[*internalpb.SearchResults]

	partitionIDsSet *typeutil.ConcurrentSet[UniqueID]

	mixCoord        types.MixCoordClient
	node            types.ProxyComponent
	lb              LBPolicy
	queryChannelsTs map[string]Timestamp
	queryInfos      []*planpb.QueryInfo
	relatedDataSize int64

	// New reranker functions
	functionScore *rerank.FunctionScore
	rankParams    *rankParams

	isIterator bool
	// we always remove pk field from output fields, as search result already contains pk field.
	// if the user explicitly set pk field in output fields, we add it back to the result.
	userRequestedPkFieldExplicitly bool
}

func (t *searchTask) CanSkipAllocTimestamp() bool {
	var consistencyLevel commonpb.ConsistencyLevel
	useDefaultConsistency := t.request.GetUseDefaultConsistency()
	if !useDefaultConsistency {
		// legacy SDK & restful behavior
		if t.request.GetConsistencyLevel() == commonpb.ConsistencyLevel_Strong && t.request.GetGuaranteeTimestamp() > 0 {
			return true
		}
		consistencyLevel = t.request.GetConsistencyLevel()
	} else {
		collID, err := globalMetaCache.GetCollectionID(context.Background(), t.request.GetDbName(), t.request.GetCollectionName())
		if err != nil { // err is not nil if collection not exists
			log.Ctx(t.ctx).Warn("search task get collectionID failed, can't skip alloc timestamp",
				zap.String("collectionName", t.request.GetCollectionName()), zap.Error(err))
			return false
		}

		collectionInfo, err2 := globalMetaCache.GetCollectionInfo(context.Background(), t.request.GetDbName(), t.request.GetCollectionName(), collID)
		if err2 != nil {
			log.Ctx(t.ctx).Warn("search task get collection info failed, can't skip alloc timestamp",
				zap.String("collectionName", t.request.GetCollectionName()), zap.Error(err))
			return false
		}
		consistencyLevel = collectionInfo.consistencyLevel
	}
	return consistencyLevel != commonpb.ConsistencyLevel_Strong
}

func (t *searchTask) PreExecute(ctx context.Context) error {
	ctx, sp := otel.Tracer(typeutil.ProxyRole).Start(ctx, "Proxy-Search-PreExecute")
	defer sp.End()

	t.SearchRequest.IsAdvanced = len(t.request.GetSubReqs()) > 0
	t.Base.MsgType = commonpb.MsgType_Search
	t.Base.SourceID = paramtable.GetNodeID()

	collectionName := t.request.CollectionName
	t.collectionName = collectionName
	collID, err := globalMetaCache.GetCollectionID(ctx, t.request.GetDbName(), collectionName)
	if err != nil { // err is not nil if collection not exists
		return merr.WrapErrAsInputErrorWhen(err, merr.ErrCollectionNotFound, merr.ErrDatabaseNotFound)
	}

	t.SearchRequest.DbID = 0 // todo
	t.SearchRequest.CollectionID = collID
	log := log.Ctx(ctx).With(zap.Int64("collID", collID), zap.String("collName", collectionName))
	t.schema, err = globalMetaCache.GetCollectionSchema(ctx, t.request.GetDbName(), collectionName)
	if err != nil {
		log.Warn("get collection schema failed", zap.Error(err))
		return err
	}

	t.partitionKeyMode, err = isPartitionKeyMode(ctx, t.request.GetDbName(), collectionName)
	if err != nil {
		log.Warn("is partition key mode failed", zap.Error(err))
		return err
	}
	if t.partitionKeyMode && len(t.request.GetPartitionNames()) != 0 {
		return errors.New("not support manually specifying the partition names if partition key mode is used")
	}
	if t.mustUsePartitionKey && !t.partitionKeyMode {
		return merr.WrapErrAsInputError(merr.WrapErrParameterInvalidMsg("must use partition key in the search request " +
			"because the mustUsePartitionKey config is true"))
	}

	if !t.partitionKeyMode && len(t.request.GetPartitionNames()) > 0 {
		// translate partition name to partition ids. Use regex-pattern to match partition name.
		t.SearchRequest.PartitionIDs, err = getPartitionIDs(ctx, t.request.GetDbName(), collectionName, t.request.GetPartitionNames())
		if err != nil {
			log.Warn("failed to get partition ids", zap.Error(err))
			return err
		}
	}

	t.translatedOutputFields, t.userOutputFields, t.userDynamicFields, t.userRequestedPkFieldExplicitly, err = translateOutputFields(t.request.OutputFields, t.schema, true)
	if err != nil {
		log.Warn("translate output fields failed", zap.Error(err), zap.Any("schema", t.schema))
		return err
	}
	log.Debug("translate output fields",
		zap.Strings("output fields", t.translatedOutputFields))

	if t.SearchRequest.GetIsAdvanced() {
		if len(t.request.GetSubReqs()) > defaultMaxSearchRequest {
			return errors.New(fmt.Sprintf("maximum of ann search requests is %d", defaultMaxSearchRequest))
		}
	}

	nq, err := t.checkNq(ctx)
	if err != nil {
		log.Info("failed to check nq", zap.Error(err))
		return err
	}
	t.SearchRequest.Nq = nq

	if t.SearchRequest.IgnoreGrowing, err = isIgnoreGrowing(t.request.SearchParams); err != nil {
		return err
	}

	outputFieldIDs, err := getOutputFieldIDs(t.schema, t.translatedOutputFields)
	if err != nil {
		log.Info("fail to get output field ids", zap.Error(err))
		return err
	}
	t.SearchRequest.OutputFieldsId = outputFieldIDs

	// Currently, we get vectors by requery. Once we support getting vectors from search,
	// searches with small result size could no longer need requery.
	if t.SearchRequest.GetIsAdvanced() {
		err = t.initAdvancedSearchRequest(ctx)
	} else {
		err = t.initSearchRequest(ctx)
	}
	if err != nil {
		log.Debug("init search request failed", zap.Error(err))
		return err
	}

	collectionInfo, err2 := globalMetaCache.GetCollectionInfo(ctx, t.request.GetDbName(), collectionName, t.CollectionID)
	if err2 != nil {
		log.Warn("Proxy::searchTask::PreExecute failed to GetCollectionInfo from cache",
			zap.String("collectionName", collectionName), zap.Int64("collectionID", t.CollectionID), zap.Error(err2))
		return err2
	}
	guaranteeTs := t.request.GetGuaranteeTimestamp()
	var consistencyLevel commonpb.ConsistencyLevel
	useDefaultConsistency := t.request.GetUseDefaultConsistency()
	if useDefaultConsistency {
		consistencyLevel = collectionInfo.consistencyLevel
		guaranteeTs = parseGuaranteeTsFromConsistency(guaranteeTs, t.BeginTs(), consistencyLevel)
	} else {
		consistencyLevel = t.request.GetConsistencyLevel()
		// Compatibility logic, parse guarantee timestamp
		if consistencyLevel == 0 && guaranteeTs > 0 {
			guaranteeTs = parseGuaranteeTs(guaranteeTs, t.BeginTs())
		} else {
			// parse from guarantee timestamp and user input consistency level
			guaranteeTs = parseGuaranteeTsFromConsistency(guaranteeTs, t.BeginTs(), consistencyLevel)
		}
	}

	// use collection schema updated timestamp if it's greater than calculate guarantee timestamp
	// this make query view updated happens before new read request happens
	// see also schema change design
	if collectionInfo.updateTimestamp > guaranteeTs {
		guaranteeTs = collectionInfo.updateTimestamp
	}

	t.SearchRequest.GuaranteeTimestamp = guaranteeTs
	t.SearchRequest.ConsistencyLevel = consistencyLevel
	if t.isIterator && t.request.GetGuaranteeTimestamp() > 0 {
		t.MvccTimestamp = t.request.GetGuaranteeTimestamp()
		t.GuaranteeTimestamp = t.request.GetGuaranteeTimestamp()
	}
	t.SearchRequest.IsIterator = t.isIterator

	if deadline, ok := t.TraceCtx().Deadline(); ok {
		t.SearchRequest.TimeoutTimestamp = tsoutil.ComposeTSByTime(deadline, 0)
	}

	// Set username of this search request for feature like task scheduling.
	if username, _ := GetCurUserFromContext(ctx); username != "" {
		t.SearchRequest.Username = username
	}

	if collectionInfo.collectionTTL != 0 {
		physicalTime := tsoutil.PhysicalTime(t.GetBase().GetTimestamp())
		expireTime := physicalTime.Add(-time.Duration(collectionInfo.collectionTTL))
		t.CollectionTtlTimestamps = tsoutil.ComposeTSByTime(expireTime, 0)
		// preventing overflow, abort
		if t.CollectionTtlTimestamps > t.GetBase().GetTimestamp() {
			return merr.WrapErrServiceInternal(fmt.Sprintf("ttl timestamp overflow, base timestamp: %d, ttl duration %v", t.GetBase().GetTimestamp(), collectionInfo.collectionTTL))
		}
	}

	t.resultBuf = typeutil.NewConcurrentSet[*internalpb.SearchResults]()

	if err = ValidateTask(t); err != nil {
		return err
	}

	log.Debug("search PreExecute done.",
		zap.Uint64("guarantee_ts", guaranteeTs),
		zap.Bool("use_default_consistency", useDefaultConsistency),
		zap.Any("consistency level", consistencyLevel),
		zap.Uint64("timeout_ts", t.SearchRequest.GetTimeoutTimestamp()),
		zap.Uint64("collection_ttl_timestamps", t.CollectionTtlTimestamps))
	return nil
}

func (t *searchTask) checkNq(ctx context.Context) (int64, error) {
	var nq int64
	if t.SearchRequest.GetIsAdvanced() {
		// In the context of Advanced Search, it is essential to verify that the number of vectors
		// for each individual search, denoted as nq, remains consistent.
		nq = t.request.GetNq()
		for _, req := range t.request.GetSubReqs() {
			subNq, err := getNqFromSubSearch(req)
			if err != nil {
				return 0, err
			}
			req.Nq = subNq
			if nq == 0 {
				nq = subNq
				continue
			}
			if subNq != nq {
				err = merr.WrapErrParameterInvalid(nq, subNq, "sub search request nq should be the same")
				return 0, err
			}
		}
		t.request.Nq = nq
	} else {
		var err error
		nq, err = getNq(t.request)
		if err != nil {
			return 0, err
		}
		t.request.Nq = nq
	}

	// Check if nq is valid:
	// https://milvus.io/docs/limitations.md
	if err := validateNQLimit(nq); err != nil {
		return 0, fmt.Errorf("%s [%d] is invalid, %w", NQKey, nq, err)
	}
	return nq, nil
}

func setQueryInfoIfMvEnable(queryInfo *planpb.QueryInfo, t *searchTask, plan *planpb.PlanNode) error {
	if t.enableMaterializedView {
		partitionKeyFieldSchema, err := typeutil.GetPartitionKeyFieldSchema(t.schema.CollectionSchema)
		if err != nil {
			log.Ctx(t.ctx).Warn("failed to get partition key field schema", zap.Error(err))
			return err
		}
		if typeutil.IsFieldDataTypeSupportMaterializedView(partitionKeyFieldSchema) {
			collInfo, colErr := globalMetaCache.GetCollectionInfo(t.ctx, t.request.GetDbName(), t.collectionName, t.CollectionID)
			if colErr != nil {
				log.Ctx(t.ctx).Warn("failed to get collection info", zap.Error(colErr))
				return err
			}

			if collInfo.partitionKeyIsolation {
				expr, err := exprutil.ParseExprFromPlan(plan)
				if err != nil {
					log.Ctx(t.ctx).Warn("failed to parse expr from plan during MV", zap.Error(err))
					return err
				}
				err = exprutil.ValidatePartitionKeyIsolation(expr)
				if err != nil {
					return err
				}
				// force set hints to disable
				queryInfo.Hints = "disable"
			}
			queryInfo.MaterializedViewInvolved = true
		} else {
			return errors.New("partition key field data type is not supported in materialized view")
		}
	}
	return nil
}

func (t *searchTask) initAdvancedSearchRequest(ctx context.Context) error {
	ctx, sp := otel.Tracer(typeutil.ProxyRole).Start(ctx, "init advanced search request")
	defer sp.End()
	t.partitionIDsSet = typeutil.NewConcurrentSet[UniqueID]()
	log := log.Ctx(ctx).With(zap.Int64("collID", t.GetCollectionID()), zap.String("collName", t.collectionName))
	var err error
	// TODO: Use function score uniformly to implement related logic
	if t.request.FunctionScore != nil {
		if t.functionScore, err = rerank.NewFunctionScore(t.schema.CollectionSchema, t.request.FunctionScore); err != nil {
			log.Warn("Failed to create function score", zap.Error(err))
			return err
		}
	} else {
		if t.functionScore, err = rerank.NewFunctionScoreWithlegacy(t.schema.CollectionSchema, t.request.GetSearchParams()); err != nil {
			log.Warn("Failed to create function by legacy info", zap.Error(err))
			return err
		}
	}

	t.needRequery = len(t.request.OutputFields) > 0 || len(t.functionScore.GetAllInputFieldNames()) > 0

	if t.rankParams, err = parseRankParams(t.request.GetSearchParams(), t.schema.CollectionSchema); err != nil {
		log.Error("parseRankParams failed", zap.Error(err))
		return err
	}

	if !t.functionScore.IsSupportGroup() && t.rankParams.GetGroupByFieldId() >= 0 {
		return merr.WrapErrParameterInvalidMsg("Current rerank does not support grouping search")
	}

	t.SearchRequest.SubReqs = make([]*internalpb.SubSearchRequest, len(t.request.GetSubReqs()))
	t.queryInfos = make([]*planpb.QueryInfo, len(t.request.GetSubReqs()))
	queryFieldIDs := []int64{}
	for index, subReq := range t.request.GetSubReqs() {
		plan, queryInfo, offset, _, err := t.tryGeneratePlan(subReq.GetSearchParams(), subReq.GetDsl(), subReq.GetExprTemplateValues())
		if err != nil {
			return err
		}

		ignoreGrowing := t.SearchRequest.IgnoreGrowing
		if !ignoreGrowing {
			// fetch ignore_growing from sub search param if not set in search request
			if ignoreGrowing, err = isIgnoreGrowing(subReq.GetSearchParams()); err != nil {
				return err
			}
		}

		internalSubReq := &internalpb.SubSearchRequest{
			Dsl:                subReq.GetDsl(),
			PlaceholderGroup:   subReq.GetPlaceholderGroup(),
			DslType:            subReq.GetDslType(),
			SerializedExprPlan: nil,
			Nq:                 subReq.GetNq(),
			PartitionIDs:       nil,
			Topk:               queryInfo.GetTopk(),
			Offset:             offset,
			MetricType:         queryInfo.GetMetricType(),
			GroupByFieldId:     t.rankParams.GetGroupByFieldId(),
			GroupSize:          t.rankParams.GetGroupSize(),
			IgnoreGrowing:      ignoreGrowing,
		}

		// set analyzer name for sub search
		analyzer, err := funcutil.GetAttrByKeyFromRepeatedKV("analyzer_name", subReq.GetSearchParams())
		if err == nil {
			internalSubReq.AnalyzerName = analyzer
		}

		internalSubReq.FieldId = queryInfo.GetQueryFieldId()
		queryFieldIDs = append(queryFieldIDs, internalSubReq.FieldId)
		// set PartitionIDs for sub search
		if t.partitionKeyMode {
			// isolation has tighter constraint, check first
			mvErr := setQueryInfoIfMvEnable(queryInfo, t, plan)
			if mvErr != nil {
				return mvErr
			}
			partitionIDs, err2 := t.tryParsePartitionIDsFromPlan(plan)
			if err2 != nil {
				return err2
			}
			if len(partitionIDs) > 0 {
				internalSubReq.PartitionIDs = partitionIDs
				t.partitionIDsSet.Upsert(partitionIDs...)
			}
		} else {
			internalSubReq.PartitionIDs = t.SearchRequest.GetPartitionIDs()
		}

		plan.OutputFieldIds = nil
		plan.DynamicFields = nil

		internalSubReq.SerializedExprPlan, err = proto.Marshal(plan)
		if err != nil {
			return err
		}
		if typeutil.IsFieldSparseFloatVector(t.schema.CollectionSchema, internalSubReq.FieldId) {
			metrics.ProxySearchSparseNumNonZeros.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10), t.collectionName, metrics.HybridSearchLabel, strconv.FormatInt(internalSubReq.FieldId, 10)).Observe(float64(typeutil.EstimateSparseVectorNNZFromPlaceholderGroup(internalSubReq.PlaceholderGroup, int(internalSubReq.GetNq()))))
		}
		t.SearchRequest.SubReqs[index] = internalSubReq
		t.queryInfos[index] = queryInfo
		log.Debug("proxy init search request",
			zap.Int64s("plan.OutputFieldIds", plan.GetOutputFieldIds()),
			zap.Stringer("plan", plan)) // may be very large if large term passed.
	}

	if function.HasNonBM25Functions(t.schema.CollectionSchema.Functions, queryFieldIDs) {
		ctx, sp := otel.Tracer(typeutil.ProxyRole).Start(ctx, "Proxy-AdvancedSearch-call-function-udf")
		defer sp.End()
		exec, err := function.NewFunctionExecutor(t.schema.CollectionSchema)
		if err != nil {
			return err
		}
		sp.AddEvent("Create-function-udf")
		if err := exec.ProcessSearch(ctx, t.SearchRequest); err != nil {
			return err
		}
		sp.AddEvent("Call-function-udf")
	}

	t.SearchRequest.GroupByFieldId = t.rankParams.GetGroupByFieldId()
	t.SearchRequest.GroupSize = t.rankParams.GetGroupSize()

	if t.partitionKeyMode {
		t.SearchRequest.PartitionIDs = t.partitionIDsSet.Collect()
	}

	return nil
}

func (t *searchTask) fillResult() {
	limit := t.SearchRequest.GetTopk() - t.SearchRequest.GetOffset()
	resultSizeInsufficient := false
	for _, topk := range t.result.Results.Topks {
		if topk < limit {
			resultSizeInsufficient = true
			break
		}
	}
	t.resultSizeInsufficient = resultSizeInsufficient
	t.result.CollectionName = t.collectionName
}

func (t *searchTask) initSearchRequest(ctx context.Context) error {
	ctx, sp := otel.Tracer(typeutil.ProxyRole).Start(ctx, "init search request")
	defer sp.End()

	log := log.Ctx(ctx).With(zap.Int64("collID", t.GetCollectionID()), zap.String("collName", t.collectionName))

	plan, queryInfo, offset, isIterator, err := t.tryGeneratePlan(t.request.GetSearchParams(), t.request.GetDsl(), t.request.GetExprTemplateValues())
	if err != nil {
		return err
	}

	if t.request.FunctionScore != nil {
		if t.functionScore, err = rerank.NewFunctionScore(t.schema.CollectionSchema, t.request.FunctionScore); err != nil {
			log.Warn("Failed to create function score", zap.Error(err))
			return err
		}

		if !t.functionScore.IsSupportGroup() && queryInfo.GetGroupByFieldId() > 0 {
			return merr.WrapErrParameterInvalidMsg("Rerank %s does not support grouping search", t.functionScore.RerankName())
		}
	}

	t.isIterator = isIterator
	t.SearchRequest.Offset = offset
	t.SearchRequest.FieldId = queryInfo.GetQueryFieldId()

	if t.partitionKeyMode {
		// isolation has tighter constraint, check first
		mvErr := setQueryInfoIfMvEnable(queryInfo, t, plan)
		if mvErr != nil {
			return mvErr
		}
		partitionIDs, err2 := t.tryParsePartitionIDsFromPlan(plan)
		if err2 != nil {
			return err2
		}
		if len(partitionIDs) > 0 {
			t.SearchRequest.PartitionIDs = partitionIDs
		}
	}

	vectorOutputFields := lo.Filter(t.schema.GetFields(), func(field *schemapb.FieldSchema, _ int) bool {
		return lo.Contains(t.translatedOutputFields, field.GetName()) && typeutil.IsVectorType(field.GetDataType())
	})
	t.needRequery = len(vectorOutputFields) > 0
	if t.needRequery {
		plan.OutputFieldIds = t.functionScore.GetAllInputFieldIDs()
	} else {
		primaryFieldSchema, err := t.schema.GetPkField()
		if err != nil {
			return err
		}
		allFieldIDs := typeutil.NewSet[int64](t.SearchRequest.OutputFieldsId...)
		allFieldIDs.Insert(t.functionScore.GetAllInputFieldIDs()...)
		allFieldIDs.Insert(primaryFieldSchema.FieldID)
		plan.OutputFieldIds = allFieldIDs.Collect()
		plan.DynamicFields = t.userDynamicFields
	}

	t.SearchRequest.SerializedExprPlan, err = proto.Marshal(plan)
	if err != nil {
		return err
	}
	if typeutil.IsFieldSparseFloatVector(t.schema.CollectionSchema, t.SearchRequest.FieldId) {
		metrics.ProxySearchSparseNumNonZeros.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10), t.collectionName, metrics.SearchLabel, strconv.FormatInt(t.SearchRequest.FieldId, 10)).Observe(float64(typeutil.EstimateSparseVectorNNZFromPlaceholderGroup(t.request.PlaceholderGroup, int(t.request.GetNq()))))
	}
	t.SearchRequest.PlaceholderGroup = t.request.PlaceholderGroup
	t.SearchRequest.Topk = queryInfo.GetTopk()
	t.SearchRequest.MetricType = queryInfo.GetMetricType()
	t.queryInfos = append(t.queryInfos, queryInfo)
	t.SearchRequest.DslType = commonpb.DslType_BoolExprV1
	t.SearchRequest.GroupByFieldId = queryInfo.GroupByFieldId
	t.SearchRequest.GroupSize = queryInfo.GroupSize

	if t.SearchRequest.MetricType == metric.BM25 {
		analyzer, err := funcutil.GetAttrByKeyFromRepeatedKV("analyzer_name", t.request.GetSearchParams())
		if err == nil {
			t.SearchRequest.AnalyzerName = analyzer
		}
	}

	if function.HasNonBM25Functions(t.schema.CollectionSchema.Functions, []int64{queryInfo.GetQueryFieldId()}) {
		ctx, sp := otel.Tracer(typeutil.ProxyRole).Start(ctx, "Proxy-Search-call-function-udf")
		defer sp.End()
		exec, err := function.NewFunctionExecutor(t.schema.CollectionSchema)
		if err != nil {
			return err
		}
		sp.AddEvent("Create-function-udf")
		if err := exec.ProcessSearch(ctx, t.SearchRequest); err != nil {
			return err
		}
		sp.AddEvent("Call-function-udf")
	}

	log.Debug("proxy init search request",
		zap.Int64s("plan.OutputFieldIds", plan.GetOutputFieldIds()),
		zap.Stringer("plan", plan)) // may be very large if large term passed.

	return nil
}

func (t *searchTask) tryGeneratePlan(params []*commonpb.KeyValuePair, dsl string, exprTemplateValues map[string]*schemapb.TemplateValue) (*planpb.PlanNode, *planpb.QueryInfo, int64, bool, error) {
	annsFieldName, err := funcutil.GetAttrByKeyFromRepeatedKV(AnnsFieldKey, params)
	if err != nil || len(annsFieldName) == 0 {
		vecFields := typeutil.GetVectorFieldSchemas(t.schema.CollectionSchema)
		if len(vecFields) == 0 {
			return nil, nil, 0, false, errors.New(AnnsFieldKey + " not found in schema")
		}

		if enableMultipleVectorFields && len(vecFields) > 1 {
			return nil, nil, 0, false, errors.New("multiple anns_fields exist, please specify a anns_field in search_params")
		}
		annsFieldName = vecFields[0].Name
	}
	searchInfo, err := parseSearchInfo(params, t.schema.CollectionSchema, t.rankParams)
	if err != nil {
		return nil, nil, 0, false, err
	}
	if searchInfo.collectionID > 0 && searchInfo.collectionID != t.GetCollectionID() {
		return nil, nil, 0, false, merr.WrapErrParameterInvalidMsg("collection id:%d in the request is not consistent to that in the search context,"+
			"alias or database may have been changed: %d", searchInfo.collectionID, t.GetCollectionID())
	}

	annField := typeutil.GetFieldByName(t.schema.CollectionSchema, annsFieldName)
	if searchInfo.planInfo.GetGroupByFieldId() != -1 && annField.GetDataType() == schemapb.DataType_BinaryVector {
		return nil, nil, 0, false, errors.New("not support search_group_by operation based on binary vector column")
	}

	searchInfo.planInfo.QueryFieldId = annField.GetFieldID()
	start := time.Now()
	plan, planErr := planparserv2.CreateSearchPlan(t.schema.schemaHelper, dsl, annsFieldName, searchInfo.planInfo, exprTemplateValues)
	if planErr != nil {
		log.Ctx(t.ctx).Warn("failed to create query plan", zap.Error(planErr),
			zap.String("dsl", dsl), // may be very large if large term passed.
			zap.String("anns field", annsFieldName), zap.Any("query info", searchInfo.planInfo))
		metrics.ProxyParseExpressionLatency.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10), "search", metrics.FailLabel).Observe(float64(time.Since(start).Milliseconds()))
		return nil, nil, 0, false, merr.WrapErrParameterInvalidMsg("failed to create query plan: %v", planErr)
	}
	metrics.ProxyParseExpressionLatency.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10), "search", metrics.SuccessLabel).Observe(float64(time.Since(start).Milliseconds()))
	log.Ctx(t.ctx).Debug("create query plan",
		zap.String("dsl", t.request.Dsl), // may be very large if large term passed.
		zap.String("anns field", annsFieldName), zap.Any("query info", searchInfo.planInfo))
	return plan, searchInfo.planInfo, searchInfo.offset, searchInfo.isIterator, nil
}

func (t *searchTask) tryParsePartitionIDsFromPlan(plan *planpb.PlanNode) ([]int64, error) {
	expr, err := exprutil.ParseExprFromPlan(plan)
	if err != nil {
		log.Ctx(t.ctx).Warn("failed to parse expr", zap.Error(err))
		return nil, err
	}
	partitionKeys := exprutil.ParseKeys(expr, exprutil.PartitionKey)
	hashedPartitionNames, err := assignPartitionKeys(t.ctx, t.request.GetDbName(), t.collectionName, partitionKeys)
	if err != nil {
		log.Ctx(t.ctx).Warn("failed to assign partition keys", zap.Error(err))
		return nil, err
	}

	if len(hashedPartitionNames) > 0 {
		// translate partition name to partition ids. Use regex-pattern to match partition name.
		PartitionIDs, err2 := getPartitionIDs(t.ctx, t.request.GetDbName(), t.collectionName, hashedPartitionNames)
		if err2 != nil {
			log.Ctx(t.ctx).Warn("failed to get partition ids", zap.Error(err2))
			return nil, err2
		}
		return PartitionIDs, nil
	}
	return nil, nil
}

func (t *searchTask) Execute(ctx context.Context) error {
	ctx, sp := otel.Tracer(typeutil.ProxyRole).Start(ctx, "Proxy-Search-Execute")
	defer sp.End()
	log := log.Ctx(ctx).WithLazy(zap.Int64("nq", t.SearchRequest.GetNq()))

	tr := timerecord.NewTimeRecorder(fmt.Sprintf("proxy execute search %d", t.ID()))
	defer tr.CtxElapse(ctx, "done")

	err := t.lb.Execute(ctx, CollectionWorkLoad{
		db:             t.request.GetDbName(),
		collectionID:   t.SearchRequest.CollectionID,
		collectionName: t.collectionName,
		nq:             t.Nq,
		exec:           t.searchShard,
	})
	if err != nil {
		log.Warn("search execute failed", zap.Error(err))
		return errors.Wrap(err, "failed to search")
	}

	log.Debug("Search Execute done.",
		zap.Int64("collection", t.GetCollectionID()),
		zap.Int64s("partitionIDs", t.GetPartitionIDs()))
	return nil
}

// find the last bound based on reduced results and metric type
// only support nq == 1, for search iterator v2
func getLastBound(result *milvuspb.SearchResults, incomingLastBound *float32, metricType string) float32 {
	len := len(result.Results.Scores)
	if len > 0 && result.GetResults().GetNumQueries() == 1 {
		return result.Results.Scores[len-1]
	}
	// if no results found and incoming last bound is not nil, return it
	if incomingLastBound != nil {
		return *incomingLastBound
	}
	// if no results found and it is the first call, return the closest bound
	if metric.PositivelyRelated(metricType) {
		return math.MaxFloat32
	}
	return -math.MaxFloat32
}

func (t *searchTask) PostExecute(ctx context.Context) error {
	ctx, sp := otel.Tracer(typeutil.ProxyRole).Start(ctx, "Proxy-Search-PostExecute")
	defer sp.End()

	tr := timerecord.NewTimeRecorder("searchTask PostExecute")
	defer func() {
		tr.CtxElapse(ctx, "done")
	}()
	log := log.Ctx(ctx).With(zap.Int64("nq", t.SearchRequest.GetNq()))

	toReduceResults, err := t.collectSearchResults(ctx)
	if err != nil {
		log.Warn("failed to collect search results", zap.Error(err))
		return err
	}

	t.queryChannelsTs = make(map[string]uint64)
	t.relatedDataSize = 0
	isTopkReduce := false
	isRecallEvaluation := false
	for _, r := range toReduceResults {
		if r.GetIsTopkReduce() {
			isTopkReduce = true
		}
		if r.GetIsRecallEvaluation() {
			isRecallEvaluation = true
		}
		t.relatedDataSize += r.GetCostAggregation().GetTotalRelatedDataSize()
		for ch, ts := range r.GetChannelsMvcc() {
			t.queryChannelsTs[ch] = ts
		}
	}

	t.isTopkReduce = isTopkReduce
	t.isRecallEvaluation = isRecallEvaluation

	// call pipeline
	pipeline, err := newBuiltInPipeline(t)
	if err != nil {
		log.Warn("Faild to create post process pipeline")
		return err
	}
	if t.result, err = pipeline.Run(ctx, sp, toReduceResults); err != nil {
		return err
	}
	t.fillResult()
	t.result.Results.OutputFields = t.userOutputFields
	t.result.CollectionName = t.request.GetCollectionName()

	primaryFieldSchema, _ := t.schema.GetPkField()
	if t.userRequestedPkFieldExplicitly {
		t.result.Results.OutputFields = append(t.result.Results.OutputFields, primaryFieldSchema.GetName())
		var scalars *schemapb.ScalarField
		if primaryFieldSchema.GetDataType() == schemapb.DataType_Int64 {
			scalars = &schemapb.ScalarField{
				Data: &schemapb.ScalarField_LongData{
					LongData: t.result.Results.Ids.GetIntId(),
				},
			}
		} else {
			scalars = &schemapb.ScalarField{
				Data: &schemapb.ScalarField_StringData{
					StringData: t.result.Results.Ids.GetStrId(),
				},
			}
		}
		pkFieldData := &schemapb.FieldData{
			FieldName: primaryFieldSchema.GetName(),
			FieldId:   primaryFieldSchema.GetFieldID(),
			Type:      primaryFieldSchema.GetDataType(),
			IsDynamic: false,
			Field: &schemapb.FieldData_Scalars{
				Scalars: scalars,
			},
		}
		t.result.Results.FieldsData = append(t.result.Results.FieldsData, pkFieldData)
	}
	t.result.Results.PrimaryFieldName = primaryFieldSchema.GetName()
	if t.isIterator && len(t.queryInfos) == 1 && t.queryInfos[0] != nil {
		if iterInfo := t.queryInfos[0].GetSearchIteratorV2Info(); iterInfo != nil {
			t.result.Results.SearchIteratorV2Results = &schemapb.SearchIteratorV2Results{
				Token:     iterInfo.GetToken(),
				LastBound: getLastBound(t.result, iterInfo.LastBound, getMetricType(toReduceResults)),
			}
		}
	}
	if t.isIterator && t.request.GetGuaranteeTimestamp() == 0 {
		// first page for iteration, need to set up sessionTs for iterator
		t.result.SessionTs = getMaxMvccTsFromChannels(t.queryChannelsTs, t.BeginTs())
	}

	metrics.ProxyReduceResultLatency.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10), metrics.SearchLabel).Observe(float64(tr.RecordSpan().Milliseconds()))

	log.Debug("Search post execute done",
		zap.Int64("collection", t.GetCollectionID()),
		zap.Int64s("partitionIDs", t.GetPartitionIDs()))
	return nil
}

func (t *searchTask) searchShard(ctx context.Context, nodeID int64, qn types.QueryNodeClient, channel string) error {
	searchReq := typeutil.Clone(t.SearchRequest)
	searchReq.GetBase().TargetID = nodeID
	req := &querypb.SearchRequest{
		Req:             searchReq,
		DmlChannels:     []string{channel},
		Scope:           querypb.DataScope_All,
		TotalChannelNum: int32(1),
	}

	log := log.Ctx(ctx).With(zap.Int64("collection", t.GetCollectionID()),
		zap.Int64s("partitionIDs", t.GetPartitionIDs()),
		zap.Int64("nodeID", nodeID),
		zap.String("channel", channel))

	var result *internalpb.SearchResults
	var err error

	result, err = qn.Search(ctx, req)
	if err != nil {
		log.Warn("QueryNode search return error", zap.Error(err))
		globalMetaCache.DeprecateShardCache(t.request.GetDbName(), t.collectionName)
		return err
	}
	if result.GetStatus().GetErrorCode() == commonpb.ErrorCode_NotShardLeader {
		log.Warn("QueryNode is not shardLeader")
		globalMetaCache.DeprecateShardCache(t.request.GetDbName(), t.collectionName)
		return errInvalidShardLeaders
	}
	if result.GetStatus().GetErrorCode() != commonpb.ErrorCode_Success {
		log.Warn("QueryNode search result error",
			zap.String("reason", result.GetStatus().GetReason()))
		return errors.Wrapf(merr.Error(result.GetStatus()), "fail to search on QueryNode %d", nodeID)
	}
	if t.resultBuf != nil {
		t.resultBuf.Insert(result)
	}
	t.lb.UpdateCostMetrics(nodeID, result.CostAggregation)

	return nil
}

func (t *searchTask) estimateResultSize(nq int64, topK int64) (int64, error) {
	vectorOutputFields := lo.Filter(t.schema.GetFields(), func(field *schemapb.FieldSchema, _ int) bool {
		return lo.Contains(t.translatedOutputFields, field.GetName()) && typeutil.IsVectorType(field.GetDataType())
	})
	// Currently, we get vectors by requery. Once we support getting vectors from search,
	// searches with small result size could no longer need requery.
	if len(vectorOutputFields) > 0 {
		return math.MaxInt64, nil
	}
	// If no vector field as output, no need to requery.
	return 0, nil

	//outputFields := lo.Filter(t.schema.GetFields(), func(field *schemapb.FieldSchema, _ int) bool {
	//	return lo.Contains(t.translatedOutputFields, field.GetName())
	//})
	//sizePerRecord, err := typeutil.EstimateSizePerRecord(&schemapb.CollectionSchema{Fields: outputFields})
	//if err != nil {
	//	return 0, err
	//}
	//return int64(sizePerRecord) * nq * topK, nil
}

func (t *searchTask) collectSearchResults(ctx context.Context) ([]*internalpb.SearchResults, error) {
	select {
	case <-t.TraceCtx().Done():
		log.Ctx(ctx).Warn("search task wait to finish timeout!")
		return nil, fmt.Errorf("search task wait to finish timeout, msgID=%d", t.ID())
	default:
		toReduceResults := make([]*internalpb.SearchResults, 0)
		log.Ctx(ctx).Debug("all searches are finished or canceled")
		t.resultBuf.Range(func(res *internalpb.SearchResults) bool {
			toReduceResults = append(toReduceResults, res)
			log.Ctx(ctx).Debug("proxy receives one search result",
				zap.Int64("sourceID", res.GetBase().GetSourceID()))
			return true
		})
		return toReduceResults, nil
	}
}

func (t *searchTask) TraceCtx() context.Context {
	return t.ctx
}

func (t *searchTask) ID() UniqueID {
	return t.Base.MsgID
}

func (t *searchTask) SetID(uid UniqueID) {
	t.Base.MsgID = uid
}

func (t *searchTask) Name() string {
	return SearchTaskName
}

func (t *searchTask) Type() commonpb.MsgType {
	return t.Base.MsgType
}

func (t *searchTask) BeginTs() Timestamp {
	return t.Base.Timestamp
}

func (t *searchTask) EndTs() Timestamp {
	return t.Base.Timestamp
}

func (t *searchTask) SetTs(ts Timestamp) {
	t.Base.Timestamp = ts
}

func (t *searchTask) OnEnqueue() error {
	t.Base = commonpbutil.NewMsgBase()
	t.Base.MsgType = commonpb.MsgType_Search
	t.Base.SourceID = paramtable.GetNodeID()
	return nil
}
