// Copyright (c) 2018 Uber Technologies, Inc.
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

package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/m3db/m3/src/cmd/services/m3coordinator/ingest"
	"github.com/m3db/m3/src/dbnode/client"
	"github.com/m3db/m3/src/metrics/policy"
	"github.com/m3db/m3/src/query/api/v1/handler/prometheus"
	"github.com/m3db/m3/src/query/api/v1/handler/prometheus/handleroptions"
	"github.com/m3db/m3/src/query/api/v1/options"
	"github.com/m3db/m3/src/query/api/v1/route"
	"github.com/m3db/m3/src/query/generated/proto/prompb"
	"github.com/m3db/m3/src/query/models"
	"github.com/m3db/m3/src/query/storage"
	"github.com/m3db/m3/src/query/storage/m3/storagemetadata"
	"github.com/m3db/m3/src/query/ts"
	"github.com/m3db/m3/src/query/util/logging"
	"github.com/m3db/m3/src/x/clock"
	xerrors "github.com/m3db/m3/src/x/errors"
	"github.com/m3db/m3/src/x/headers"
	"github.com/m3db/m3/src/x/instrument"
	xhttp "github.com/m3db/m3/src/x/net/http"
	"github.com/m3db/m3/src/x/retry"
	xsync "github.com/m3db/m3/src/x/sync"
	xtime "github.com/m3db/m3/src/x/time"

	"github.com/cespare/xxhash/v2"
	"github.com/golang/protobuf/proto"
	"github.com/golang/snappy"
	murmur3 "github.com/m3db/stackmurmur3/v2"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

const (
	// PromWriteURL is the url for the prom write handler
	PromWriteURL = route.Prefix + "/prom/remote/write"

	// PromWriteHTTPMethod is the HTTP method used with this resource.
	PromWriteHTTPMethod = http.MethodPost

	// emptyStoragePolicyVar for code readability.
	emptyStoragePolicyVar = ""

	// defaultForwardingTimeout is the default forwarding timeout.
	defaultForwardingTimeout = 15 * time.Second

	// maxLiteralIsTooLongLogCount is the number of times the time series labels should be logged
	// upon "literal is too long" error.
	maxLiteralIsTooLongLogCount = 10
	// literalPrefixLength is the length of the label literal prefix that is logged upon
	// "literal is too long" error.
	literalPrefixLength = 100
)

var (
	errNoDownsamplerAndWriter       = errors.New("no downsampler and writer set")
	errNoTagOptions                 = errors.New("no tag options set")
	errNoNowFn                      = errors.New("no now fn set")
	errUnaggregatedStoragePolicySet = errors.New("storage policy should not be set for unaggregated metrics")

	defaultForwardingRetryForever = false
	defaultForwardingRetryJitter  = true
	defaultForwardRetryConfig     = retry.Configuration{
		InitialBackoff: time.Second * 2,
		BackoffFactor:  2,
		MaxRetries:     1,
		Forever:        &defaultForwardingRetryForever,
		Jitter:         &defaultForwardingRetryJitter,
	}

	defaultValue = ingest.IterValue{
		Tags:       models.EmptyTags(),
		Attributes: ts.DefaultSeriesAttributes(),
		Metadata:   ts.Metadata{},
	}

	headerToMetricType = map[string]prompb.MetricType{
		"counter":         prompb.MetricType_COUNTER,
		"gauge":           prompb.MetricType_GAUGE,
		"gauge_histogram": prompb.MetricType_GAUGE_HISTOGRAM,
		"histogram":       prompb.MetricType_HISTOGRAM,
		"info":            prompb.MetricType_INFO,
		"stateset":        prompb.MetricType_STATESET,
		"summary":         prompb.MetricType_SUMMARY,
	}
)

// PromWriteHandler represents a handler for prometheus write endpoint.
type PromWriteHandler struct {
	downsamplerAndWriter   ingest.DownsamplerAndWriter
	tagOptions             models.TagOptions
	storeMetricsType       bool
	forwarding             handleroptions.PromWriteHandlerForwardingOptions
	forwardTimeout         time.Duration
	forwardHTTPClient      *http.Client
	forwardingBoundWorkers xsync.WorkerPool
	forwardContext         context.Context
	forwardRetrier         retry.Retrier
	nowFn                  clock.NowFn
	instrumentOpts         instrument.Options
	metrics                promWriteMetrics

	// Counting the number of times of "literal is too long" error for log sampling purposes.
	numLiteralIsTooLong uint32
}

// NewPromWriteHandler returns a new instance of handler.
func NewPromWriteHandler(options options.HandlerOptions) (http.Handler, error) {
	var (
		downsamplerAndWriter = options.DownsamplerAndWriter()
		tagOptions           = options.TagOptions()
		nowFn                = options.NowFn()
		forwarding           = options.Config().WriteForwarding.PromRemoteWrite
		instrumentOpts       = options.InstrumentOpts()
	)

	if downsamplerAndWriter == nil {
		return nil, errNoDownsamplerAndWriter
	}

	if tagOptions == nil {
		return nil, errNoTagOptions
	}

	if nowFn == nil {
		return nil, errNoNowFn
	}

	scope := options.InstrumentOpts().
		MetricsScope().
		Tagged(map[string]string{"handler": "remote-write"})
	metrics, err := newPromWriteMetrics(scope)
	if err != nil {
		return nil, err
	}

	// Only use a forwarding worker pool if concurrency is bound, otherwise
	// if unlimited we just spin up a goroutine for each incoming write.
	var forwardingBoundWorkers xsync.WorkerPool
	if v := forwarding.MaxConcurrency; v > 0 {
		forwardingBoundWorkers = xsync.NewWorkerPool(v)
		forwardingBoundWorkers.Init()
	}

	forwardTimeout := defaultForwardingTimeout
	if v := forwarding.Timeout; v > 0 {
		forwardTimeout = v
	}

	forwardHTTPOpts := xhttp.DefaultHTTPClientOptions()
	forwardHTTPOpts.DisableCompression = true // Already snappy compressed.
	forwardHTTPOpts.RequestTimeout = forwardTimeout

	forwardRetryConfig := defaultForwardRetryConfig
	if forwarding.Retry != nil {
		forwardRetryConfig = *forwarding.Retry
	}
	forwardRetryOpts := forwardRetryConfig.NewOptions(
		scope.SubScope("forwarding-retry"),
	)

	return &PromWriteHandler{
		downsamplerAndWriter:   downsamplerAndWriter,
		tagOptions:             tagOptions,
		storeMetricsType:       options.StoreMetricsType(),
		forwarding:             forwarding,
		forwardTimeout:         forwardTimeout,
		forwardHTTPClient:      xhttp.NewHTTPClient(forwardHTTPOpts),
		forwardingBoundWorkers: forwardingBoundWorkers,
		forwardContext:         context.Background(),
		forwardRetrier:         retry.NewRetrier(forwardRetryOpts),
		nowFn:                  nowFn,
		metrics:                metrics,
		instrumentOpts:         instrumentOpts,
	}, nil
}

type promWriteMetrics struct {
	writeSuccess             tally.Counter
	writeErrorsServer        tally.Counter
	writeErrorsClient        tally.Counter
	writeBatchLatency        tally.Histogram
	writeBatchLatencyBuckets tally.DurationBuckets
	ingestLatency            tally.Histogram
	ingestLatencyBuckets     tally.DurationBuckets
	forwardSuccess           tally.Counter
	forwardErrors            tally.Counter
	forwardDropped           tally.Counter
	forwardLatency           tally.Histogram
	forwardShadowKeep        tally.Counter
	forwardShadowDrop        tally.Counter
}

func (m *promWriteMetrics) incError(err error) {
	if xhttp.IsClientError(err) {
		m.writeErrorsClient.Inc(1)
	} else {
		m.writeErrorsServer.Inc(1)
	}
}

func newPromWriteMetrics(scope tally.Scope) (promWriteMetrics, error) {
	buckets, err := ingest.NewLatencyBuckets()
	if err != nil {
		return promWriteMetrics{}, err
	}
	return promWriteMetrics{
		writeSuccess:             scope.SubScope("write").Counter("success"),
		writeErrorsServer:        scope.SubScope("write").Tagged(map[string]string{"code": "5XX"}).Counter("errors"),
		writeErrorsClient:        scope.SubScope("write").Tagged(map[string]string{"code": "4XX"}).Counter("errors"),
		writeBatchLatency:        scope.SubScope("write").Histogram("batch-latency", buckets.WriteLatencyBuckets),
		writeBatchLatencyBuckets: buckets.WriteLatencyBuckets,
		ingestLatency:            scope.SubScope("ingest").Histogram("latency", buckets.IngestLatencyBuckets),
		ingestLatencyBuckets:     buckets.IngestLatencyBuckets,
		forwardSuccess:           scope.SubScope("forward").Counter("success"),
		forwardErrors:            scope.SubScope("forward").Counter("errors"),
		forwardDropped:           scope.SubScope("forward").Counter("dropped"),
		forwardLatency:           scope.SubScope("forward").Histogram("latency", buckets.WriteLatencyBuckets),
		forwardShadowKeep:        scope.SubScope("forward").SubScope("shadow").Counter("keep"),
		forwardShadowDrop:        scope.SubScope("forward").SubScope("shadow").Counter("drop"),
	}, nil
}

func (h *PromWriteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	batchRequestStopwatch := h.metrics.writeBatchLatency.Start()
	defer batchRequestStopwatch.Stop()

	checkedReq, err := h.checkedParseRequest(r)
	if err != nil {
		h.metrics.incError(err)
		xhttp.WriteError(w, err)
		return
	}

	var (
		req  = checkedReq.Request
		opts = checkedReq.Options
	)
	// Begin async forwarding.
	// NB(r): Be careful about not returning buffers to pool
	// if the request bodies ever get pooled until after
	// forwarding completes.
	if targets := h.forwarding.Targets; len(targets) > 0 {
		for _, target := range targets {
			target := target // Capture for lambda.
			forward := func() {
				now := h.nowFn()

				var (
					attempt = func() error {
						// Consider propagating baggage without tying
						// context to request context in future.
						ctx, cancel := context.WithTimeout(h.forwardContext, h.forwardTimeout)
						defer cancel()
						return h.forward(ctx, checkedReq, r.Header, target)
					}
					err error
				)
				if target.NoRetry {
					err = attempt()
				} else {
					err = h.forwardRetrier.Attempt(attempt)
				}

				// Record forward ingestion delay.
				// NB: this includes any time for retries.
				for _, series := range req.Timeseries {
					for _, sample := range series.Samples {
						age := now.Sub(storage.PromTimestampToTime(sample.Timestamp))
						h.metrics.forwardLatency.RecordDuration(age)
					}
				}

				if err != nil {
					h.metrics.forwardErrors.Inc(1)
					logger := logging.WithContext(h.forwardContext, h.instrumentOpts)
					logger.Error("forward error", zap.Error(err))
					return
				}

				h.metrics.forwardSuccess.Inc(1)
			}

			spawned := false
			if h.forwarding.MaxConcurrency > 0 {
				spawned = h.forwardingBoundWorkers.GoIfAvailable(forward)
			} else {
				go forward()
				spawned = true
			}
			if !spawned {
				h.metrics.forwardDropped.Inc(1)
			}
		}
	}

	batchErr := h.write(r.Context(), req, opts)

	// Record ingestion delay latency
	now := h.nowFn()
	for _, series := range req.Timeseries {
		for _, sample := range series.Samples {
			age := now.Sub(storage.PromTimestampToTime(sample.Timestamp))
			h.metrics.ingestLatency.RecordDuration(age)
		}
	}

	if batchErr != nil {
		var (
			errs                 = batchErr.Errors()
			lastRegularErr       string
			lastBadRequestErr    string
			numRegular           int
			numBadRequest        int
			numResourceExhausted int
		)
		for _, err := range errs {
			switch {
			case client.IsResourceExhaustedError(err):
				numResourceExhausted++
				lastBadRequestErr = err.Error()
			case client.IsBadRequestError(err):
				numBadRequest++
				lastBadRequestErr = err.Error()
			case xerrors.IsInvalidParams(err):
				numBadRequest++
				lastBadRequestErr = err.Error()
			default:
				numRegular++
				lastRegularErr = err.Error()
			}
		}

		var status int
		switch {
		case numBadRequest == len(errs):
			status = http.StatusBadRequest
		case numResourceExhausted > 0:
			status = http.StatusTooManyRequests
		default:
			status = http.StatusInternalServerError
		}

		logger := logging.WithContext(r.Context(), h.instrumentOpts)
		logger.Error("write error",
			zap.String("remoteAddr", r.RemoteAddr),
			zap.Int("httpResponseStatusCode", status),
			zap.Int("numResourceExhaustedErrors", numResourceExhausted),
			zap.Int("numRegularErrors", numRegular),
			zap.Int("numBadRequestErrors", numBadRequest),
			zap.String("lastRegularError", lastRegularErr),
			zap.String("lastBadRequestErr", lastBadRequestErr))

		var resultErrMessage string
		if lastRegularErr != "" {
			resultErrMessage = fmt.Sprintf("retryable_errors: count=%d, last=%s",
				numRegular, lastRegularErr)
		}
		if lastBadRequestErr != "" {
			var sep string
			if lastRegularErr != "" {
				sep = ", "
			}
			resultErrMessage = fmt.Sprintf("%s%sbad_request_errors: count=%d, last=%s",
				resultErrMessage, sep, numBadRequest, lastBadRequestErr)
		}

		resultError := xhttp.NewError(errors.New(resultErrMessage), status)
		h.metrics.incError(resultError)
		xhttp.WriteError(w, resultError)
		return
	}

	// NB(schallert): this is frustrating but if we don't explicitly write an HTTP
	// status code (or via Write()), OpenTracing middleware reports code=0 and
	// shows up as error.
	w.WriteHeader(200)
	h.metrics.writeSuccess.Inc(1)
}

type parseRequestResult struct {
	Request        *prompb.WriteRequest
	Options        ingest.WriteOptions
	CompressResult prometheus.ParsePromCompressedRequestResult
}

func (h *PromWriteHandler) checkedParseRequest(
	r *http.Request,
) (parseRequestResult, error) {
	result, err := h.parseRequest(r)
	if err != nil {
		// Always invalid request if parsing fails params.
		return parseRequestResult{}, xerrors.NewInvalidParamsError(err)
	}
	return result, nil
}

// parseRequest extracts the Prometheus write request from the request body and
// headers. WARNING: it is not guaranteed that the tags returned in the request
// body are in sorted order. It is expected that the caller ensures the tags are
// sorted before passing them to storage, which currently happens in write() ->
// newTSPromIter() -> storage.PromLabelsToM3Tags() -> tags.AddTags(). This is
// the only path written metrics are processed, but future write paths must
// uphold the same guarantees.
func (h *PromWriteHandler) parseRequest(
	r *http.Request,
) (parseRequestResult, error) {
	var opts ingest.WriteOptions
	if v := strings.TrimSpace(r.Header.Get(headers.MetricsTypeHeader)); v != "" {
		// Allow the metrics type and storage policies to override
		// the default rules and policies if specified.
		metricsType, err := storagemetadata.ParseMetricsType(v)
		if err != nil {
			return parseRequestResult{}, err
		}

		// Ensure ingest options specify we are overriding the
		// downsampling rules with zero rules to be applied (so
		// only direct writes will be made).
		opts.DownsampleOverride = true
		opts.DownsampleMappingRules = nil

		strPolicy := strings.TrimSpace(r.Header.Get(headers.MetricsStoragePolicyHeader))
		switch metricsType {
		case storagemetadata.UnaggregatedMetricsType:
			if strPolicy != emptyStoragePolicyVar {
				return parseRequestResult{}, errUnaggregatedStoragePolicySet
			}
		default:
			parsed, err := policy.ParseStoragePolicy(strPolicy)
			if err != nil {
				err = fmt.Errorf("could not parse storage policy: %v", err)
				return parseRequestResult{}, err
			}

			// Make sure this specific storage policy is used for the writes.
			opts.WriteOverride = true
			opts.WriteStoragePolicies = policy.StoragePolicies{
				parsed,
			}
		}
	}
	if v := strings.TrimSpace(r.Header.Get(headers.WriteTypeHeader)); v != "" {
		switch v {
		case headers.DefaultWriteType:
		case headers.AggregateWriteType:
			opts.WriteOverride = true
			opts.WriteStoragePolicies = policy.StoragePolicies{}
		default:
			err := fmt.Errorf("unrecognized write type: %s", v)
			return parseRequestResult{}, err
		}
	}

	result, err := prometheus.ParsePromCompressedRequest(r)
	if err != nil {
		return parseRequestResult{}, err
	}

	var req prompb.WriteRequest
	if err := proto.Unmarshal(result.UncompressedBody, &req); err != nil {
		return parseRequestResult{}, err
	}

	if mapStr := r.Header.Get(headers.MapTagsByJSONHeader); mapStr != "" {
		var opts handleroptions.MapTagsOptions
		if err := json.Unmarshal([]byte(mapStr), &opts); err != nil {
			return parseRequestResult{}, err
		}

		if err := mapTags(&req, opts); err != nil {
			return parseRequestResult{}, err
		}
	}

	if promType := r.Header.Get(headers.PromTypeHeader); promType != "" {
		tp, ok := headerToMetricType[strings.ToLower(promType)]
		if !ok {
			return parseRequestResult{}, fmt.Errorf("unknown prom metric type %s", promType)
		}
		for i := range req.Timeseries {
			req.Timeseries[i].Type = tp
		}
	}

	// Check if any of the labels exceed literal length limits and occasionally print them
	// in a log message for debugging purposes.
	maxTagLiteralLength := int(h.tagOptions.MaxTagLiteralLength())
	for _, ts := range req.Timeseries {
		for _, l := range ts.Labels {
			if len(l.Name) > maxTagLiteralLength || len(l.Value) > maxTagLiteralLength {
				h.maybeLogLabelsWithTooLongLiterals(h.instrumentOpts.Logger(), l)
				err := fmt.Errorf("label literal is too long: nameLength=%d, valueLength=%d, maxLength=%d",
					len(l.Name), len(l.Value), maxTagLiteralLength)
				return parseRequestResult{}, err
			}
		}
	}

	return parseRequestResult{
		Request:        &req,
		Options:        opts,
		CompressResult: result,
	}, nil
}

func (h *PromWriteHandler) write(
	ctx context.Context,
	r *prompb.WriteRequest,
	opts ingest.WriteOptions,
) ingest.BatchError {
	iter, err := newPromTSIter(r.Timeseries, h.tagOptions, h.storeMetricsType)
	if err != nil {
		var errs xerrors.MultiError
		return errs.Add(err)
	}
	return h.downsamplerAndWriter.WriteBatch(ctx, iter, opts)
}

func (h *PromWriteHandler) forward(
	ctx context.Context,
	res parseRequestResult,
	header http.Header,
	target handleroptions.PromWriteHandlerForwardTargetOptions,
) error {
	body := bytes.NewReader(res.CompressResult.CompressedBody)
	if shadowOpts := target.Shadow; shadowOpts != nil {
		// Need to send a subset of the original series to the shadow target.
		buffer, err := h.buildForwardShadowRequestBody(res, shadowOpts)
		if err != nil {
			return err
		}
		// Read the body from the shadow request body just built.
		body.Reset(buffer)
	}

	method := target.Method
	if method == "" {
		method = http.MethodPost
	}
	url := target.URL
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}

	// There are multiple headers that impact coordinator behavior on the write
	// (map tags, storage policy, etc.) that we must forward to the target
	// coordinator to guarantee same behavior as the coordinator that originally
	// received the request.
	if header != nil {
		for h := range header {
			if strings.HasPrefix(h, headers.M3HeaderPrefix) {
				req.Header.Add(h, header.Get(h))
			}
		}
	}

	if targetHeaders := target.Headers; targetHeaders != nil {
		// If headers set, attach to request.
		for name, value := range targetHeaders {
			req.Header.Add(name, value)
		}
	}

	resp, err := h.forwardHTTPClient.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		response, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			response = []byte(fmt.Sprintf("error reading body: %v", err))
		}
		return fmt.Errorf("expected status code 2XX: actual=%v, method=%v, url=%v, resp=%s",
			resp.StatusCode, method, url, response)
	}

	return nil
}

func (h *PromWriteHandler) buildForwardShadowRequestBody(
	res parseRequestResult,
	shadowOpts *handleroptions.PromWriteHandlerForwardTargetShadowOptions,
) ([]byte, error) {
	if shadowOpts.Percent < 0 || shadowOpts.Percent > 1 {
		return nil, fmt.Errorf("forwarding shadow percent out of range [0,1]: %f",
			shadowOpts.Percent)
	}

	// Need to apply shadow percent.
	var hash func([]byte) uint64
	switch shadowOpts.Hash {
	case "":
		fallthrough
	case "xxhash":
		hash = xxhash.Sum64
	case "murmur3":
		hash = murmur3.Sum64
	default:
		return nil, fmt.Errorf("unknown hash function: %s", shadowOpts.Hash)
	}

	var (
		shadowReq = &prompb.WriteRequest{}
		labels    []prompb.Label
		buffer    []byte
	)
	for _, ts := range res.Request.Timeseries {
		// Build an ID of the series to hash.
		// First take copy of labels so the call to sort doesn't modify the
		// original slice.
		labels = append(labels[:0], ts.Labels...)
		buffer = buildPseudoIDWithLabelsLikelySorted(labels, buffer[:0])

		// Use a range of 10k to allow for setting 0.01% having an effect
		// when shadow percent is set (i.e. with percent=0.0001)
		if hash(buffer)%10000 <= uint64(shadowOpts.Percent*10000) {
			// Keep this series, it falls below the volume target of shards.
			h.metrics.forwardShadowKeep.Inc(1)
			continue
		}

		h.metrics.forwardShadowDrop.Inc(1)

		// Skip forwarding this series, not in shadow volume of shards.
		// Swap it with the tail and continue.
		shadowReq.Timeseries = append(shadowReq.Timeseries, ts)
	}

	encoded, err := proto.Marshal(shadowReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal forwarding shadow request: %w", err)
	}

	return snappy.Encode(buffer[:0], encoded), nil
}

// buildPseudoIDWithLabelsLikelySorted will build a pseudo ID that can be
// hashed/etc (but not used as primary key since not escaped), it expects the
// input labels to be likely sorted (so can avoid invoking sort in the regular
// case where series have labels already sorted when sent to remote write
// endpoint, which is commonly the case).
func buildPseudoIDWithLabelsLikelySorted(
	labels []prompb.Label,
	buffer []byte,
) []byte {
	for i, l := range labels {
		if i > 0 && bytes.Compare(l.Name, labels[i-1].Name) < 0 {
			// Sort.
			sort.Sort(sortableLabels(labels))
			// Rebuild.
			return buildPseudoIDWithLabelsLikelySorted(labels, buffer[:0])
		}
		buffer = append(buffer, l.Name...)
		buffer = append(buffer, '=')
		buffer = append(buffer, l.Value...)
		if i < len(labels)-1 {
			buffer = append(buffer, ',')
		}
	}
	return buffer
}

func (h *PromWriteHandler) maybeLogLabelsWithTooLongLiterals(logger *zap.Logger, label prompb.Label) {
	if atomic.AddUint32(&h.numLiteralIsTooLong, 1) > maxLiteralIsTooLongLogCount {
		return
	}

	safePrefix := func(b []byte, l int) []byte {
		if len(b) <= l {
			return b
		}
		return b[:l]
	}

	logger.Warn("label exceeds literal length limits",
		zap.String("namePrefix", string(safePrefix(label.Name, literalPrefixLength))),
		zap.Int("nameLength", len(label.Name)),
		zap.String("valuePrefix", string(safePrefix(label.Value, literalPrefixLength))),
		zap.Int("valueLength", len(label.Value)),
	)
}

func newPromTSIter(
	timeseries []prompb.TimeSeries,
	tagOpts models.TagOptions,
	storeMetricsType bool,
) (*promTSIter, error) {
	// Construct the tags and datapoints upfront so that if the iterator
	// is reset, we don't have to generate them twice.
	var (
		tags             = make([]models.Tags, 0, len(timeseries))
		datapoints       = make([]ts.Datapoints, 0, len(timeseries))
		seriesAttributes = make([]ts.SeriesAttributes, 0, len(timeseries))
	)

	graphiteTagOpts := tagOpts.SetIDSchemeType(models.TypeGraphite)
	for _, promTS := range timeseries {
		attributes, err := storage.PromTimeSeriesToSeriesAttributes(promTS)
		if err != nil {
			return nil, err
		}

		// Set the tag options based on the incoming source.
		opts := tagOpts
		if attributes.Source == ts.SourceTypeGraphite {
			opts = graphiteTagOpts
		}

		seriesAttributes = append(seriesAttributes, attributes)
		tags = append(tags, storage.PromLabelsToM3Tags(promTS.Labels, opts))
		datapoints = append(datapoints, storage.PromSamplesToM3Datapoints(promTS.Samples))
	}

	return &promTSIter{
		attributes:       seriesAttributes,
		idx:              -1,
		tags:             tags,
		datapoints:       datapoints,
		storeMetricsType: storeMetricsType,
	}, nil
}

type promTSIter struct {
	idx        int
	err        error
	attributes []ts.SeriesAttributes
	tags       []models.Tags
	datapoints []ts.Datapoints
	metadatas  []ts.Metadata
	annotation []byte

	storeMetricsType bool
}

func (i *promTSIter) Next() bool {
	if i.err != nil {
		return false
	}

	i.idx++
	if i.idx >= len(i.tags) {
		return false
	}

	if !i.storeMetricsType {
		return true
	}

	annotationPayload, err := storage.SeriesAttributesToAnnotationPayload(i.attributes[i.idx])
	if err != nil {
		i.err = err
		return false
	}

	i.annotation, err = annotationPayload.Marshal()
	if err != nil {
		i.err = err
		return false
	}

	if len(i.annotation) == 0 {
		i.annotation = nil
	}

	return true
}

func (i *promTSIter) Current() ingest.IterValue {
	if len(i.tags) == 0 || i.idx < 0 || i.idx >= len(i.tags) {
		return defaultValue
	}

	value := ingest.IterValue{
		Tags:       i.tags[i.idx],
		Datapoints: i.datapoints[i.idx],
		Attributes: i.attributes[i.idx],
		Unit:       xtime.Millisecond,
		Annotation: i.annotation,
	}
	if i.idx < len(i.metadatas) {
		value.Metadata = i.metadatas[i.idx]
	}
	return value
}

func (i *promTSIter) Reset() error {
	i.idx = -1
	i.err = nil
	i.annotation = nil

	return nil
}

func (i *promTSIter) Error() error {
	return i.err
}

func (i *promTSIter) SetCurrentMetadata(metadata ts.Metadata) {
	if len(i.metadatas) == 0 {
		i.metadatas = make([]ts.Metadata, len(i.tags))
	}
	if i.idx < 0 || i.idx >= len(i.metadatas) {
		return
	}
	i.metadatas[i.idx] = metadata
}

type sortableLabels []prompb.Label

func (t sortableLabels) Len() int      { return len(t) }
func (t sortableLabels) Swap(i, j int) { t[i], t[j] = t[j], t[i] }
func (t sortableLabels) Less(i, j int) bool {
	return bytes.Compare(t[i].Name, t[j].Name) == -1
}
