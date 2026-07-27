package biz

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/ringbuffer"
	"github.com/looplj/axonhub/internal/pkg/xtime"
)

const (
	// defaultPerformanceWindowSize is the default size of the sliding window in seconds (10 minutes).
	defaultPerformanceWindowSize = 600

	// MinLatencyMs is the minimum latency value (10ms) used for tokens/second calculations.
	// This matches the frontend standard MINIMUM_LATENCY_MS_FOR_CACHE_HITS.
	MinLatencyMs = 10
)

// ClampLatency enforces the minimum latency value to prevent extreme TPS calculations.
// Returns the latency if it's >= MinLatencyMs, otherwise returns MinLatencyMs.
func ClampLatency(latencyMs int64) int64 {
	if latencyMs < MinLatencyMs {
		return MinLatencyMs
	}

	return latencyMs
}

// channelMetrics holds the performance metrics for a channel in memory.
type channelMetrics struct {
	channelID int

	// sliding window of metrics for the last N minutes using ring buffer for O(1) cleanup
	window *ringbuffer.RingBuffer[*timeSlotMetrics]

	// aggregatedMetrics holds accumulated metrics for the flush period
	aggregatedMetrics *AggregatedMetrics
}

// loadChannelPerformances loads recent channel failures from request_execution
// to initialize the in-memory circuit-breaker state after a restart.
func (svc *ChannelService) loadChannelPerformances(ctx context.Context) error {
	client := svc.entFromContext(ctx)

	// Query last 6 hours of request execution data
	since := xtime.UTCNow().Add(-6 * time.Hour)

	// Fetch all channel metrics in a single GROUP BY query
	metrics, err := svc.loadAllChannelMetricsFromExecutions(ctx, client, since)
	if err != nil {
		return fmt.Errorf("failed to load channel metrics: %w", err)
	}

	if len(metrics) == 0 {
		log.Info(ctx, "No request execution data found in the last 6 hours")
		return nil
	}

	svc.channelPerfMetricsLock.Lock()
	defer svc.channelPerfMetricsLock.Unlock()

	if svc.channelPerfMetrics == nil {
		svc.channelPerfMetrics = make(map[int]*channelMetrics)
	}

	for channelID, m := range metrics {
		cm := newChannelMetrics(channelID)
		svc.populateChannelMetrics(cm, m)
		svc.channelPerfMetrics[channelID] = cm
	}

	log.Info(ctx, "Loaded channel performance metrics from request executions",
		log.Int("count", len(metrics)),
	)

	return nil
}

// channelMetricsResult holds aggregated metrics for a single channel.
// Only includes fields needed for load balancing.
type channelMetricsResult struct {
	ChannelID     int        `json:"channel_id"`
	LastFailureAt *time.Time `json:"last_failure_at"`
}

// loadAllChannelMetricsFromExecutions loads the latest failure per channel.
// Fetching typed entities avoids dialect-specific aggregate timestamp scans
// (SQLite returns MAX(datetime) as text, while PostgreSQL returns time.Time).
func (svc *ChannelService) loadAllChannelMetricsFromExecutions(ctx context.Context, client *ent.Client, since time.Time) (map[int]*channelMetricsResult, error) {
	type channelIDResult struct {
		ChannelID int `json:"channel_id"`
	}

	var channelIDs []channelIDResult

	err := client.RequestExecution.Query().
		Where(
			requestexecution.CreatedAtGTE(since),
			requestexecution.ChannelIDNotNil(),
			requestexecution.StatusEQ(requestexecution.StatusFailed),
		).
		Unique(true).
		Select(requestexecution.FieldChannelID).
		Scan(ctx, &channelIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to query channels with recent failures: %w", err)
	}

	metricsMap := make(map[int]*channelMetricsResult, len(channelIDs))
	for _, row := range channelIDs {
		latestFailure, err := client.RequestExecution.Query().
			Where(
				requestexecution.CreatedAtGTE(since),
				requestexecution.ChannelIDEQ(row.ChannelID),
				requestexecution.StatusEQ(requestexecution.StatusFailed),
			).
			Order(requestexecution.ByCreatedAt(sql.OrderDesc())).
			First(ctx)
		if ent.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to query latest failure for channel %d: %w", row.ChannelID, err)
		}

		metricsMap[row.ChannelID] = &channelMetricsResult{
			ChannelID:     row.ChannelID,
			LastFailureAt: &latestFailure.CreatedAt,
		}
	}

	return metricsMap, nil
}

// populateChannelMetrics populates channelMetrics from the aggregated result.
// Only populates fields needed for load balancing.
func (svc *ChannelService) populateChannelMetrics(cm *channelMetrics, m *channelMetricsResult) {
	if m.LastFailureAt != nil {
		cm.aggregatedMetrics.LastFailureAt = m.LastFailureAt
	}

	// Note: ConsecutiveFailures is not loaded from historical data.
	// It will be tracked in real-time as requests are processed.
}

// timeSlotMetrics holds metrics for a specific second.
type timeSlotMetrics struct {
	metricsRecord

	timestamp int64
}

type metricsRecord struct {
	SuccessCount int64
	FailureCount int64

	// ConsecutiveFailures tracks the number of consecutive failures
	// Reset to 0 on success, incremented on failure
	ConsecutiveFailures int64
}

// AggregatedMetrics holds accumulated metrics for the flush period.
type AggregatedMetrics struct {
	metricsRecord

	// RequestCount is deliberately a fair new-session selection counter, not a
	// request/attempt counter. It is advanced only by IncrementChannelSelection
	// and is never coupled to per-request performance-slot cleanup.
	RequestCount int64

	LastSelectedAt *time.Time
	LastFailureAt  *time.Time

	// StreamingFirstTokenLatencyEWMA is the EWMA of first-token latency for streaming requests.
	StreamingFirstTokenLatencyEWMA float64
	// StreamingTokensPerSecondEWMA is the EWMA of completion throughput for streaming requests.
	StreamingTokensPerSecondEWMA float64
	// StreamingSampleCount tracks streaming samples recorded for latency-aware scoring.
	StreamingSampleCount int64
	// NonStreamingLatencyEWMA is the EWMA of total request latency for non-streaming requests.
	NonStreamingLatencyEWMA float64
	// NonStreamingSampleCount tracks non-streaming samples recorded for latency-aware scoring.
	NonStreamingSampleCount int64
}

func (m *AggregatedMetrics) Clone() *AggregatedMetrics {
	return &AggregatedMetrics{
		metricsRecord:                  m.metricsRecord,
		RequestCount:                   m.RequestCount,
		LastSelectedAt:                 m.LastSelectedAt,
		LastFailureAt:                  m.LastFailureAt,
		StreamingFirstTokenLatencyEWMA: m.StreamingFirstTokenLatencyEWMA,
		StreamingTokensPerSecondEWMA:   m.StreamingTokensPerSecondEWMA,
		StreamingSampleCount:           m.StreamingSampleCount,
		NonStreamingLatencyEWMA:        m.NonStreamingLatencyEWMA,
		NonStreamingSampleCount:        m.NonStreamingSampleCount,
	}
}

// newChannelMetrics creates a new channelMetrics instance.
func newChannelMetrics(channelID int) *channelMetrics {
	cm := &channelMetrics{
		channelID: channelID,
		window:    ringbuffer.New[*timeSlotMetrics](defaultPerformanceWindowSize),
		aggregatedMetrics: &AggregatedMetrics{
			metricsRecord: metricsRecord{},
		},
	}

	return cm
}

const latencyEWMAAlpha = 0.3

// recordSuccess records a successful request to the channel metrics.
func (cm *channelMetrics) recordSuccess(slot *timeSlotMetrics, perf *PerformanceRecord) {
	slot.SuccessCount++
	cm.aggregatedMetrics.SuccessCount++
	cm.aggregatedMetrics.LastSelectedAt = &perf.EndTime

	// Reset consecutive failures on success
	cm.aggregatedMetrics.ConsecutiveFailures = 0

	firstTokenLatencyMs, requestLatencyMs, tokensPerSecond := perf.Calculate()

	if perf.Stream && perf.FirstTokenTime != nil {
		firstTokenLatency := float64(firstTokenLatencyMs)
		if cm.aggregatedMetrics.StreamingSampleCount == 0 {
			cm.aggregatedMetrics.StreamingFirstTokenLatencyEWMA = firstTokenLatency
		} else {
			cm.aggregatedMetrics.StreamingFirstTokenLatencyEWMA = latencyEWMAAlpha*firstTokenLatency + (1-latencyEWMAAlpha)*cm.aggregatedMetrics.StreamingFirstTokenLatencyEWMA
		}

		if tokensPerSecond > 0 {
			if cm.aggregatedMetrics.StreamingSampleCount == 0 {
				cm.aggregatedMetrics.StreamingTokensPerSecondEWMA = tokensPerSecond
			} else {
				cm.aggregatedMetrics.StreamingTokensPerSecondEWMA = latencyEWMAAlpha*tokensPerSecond + (1-latencyEWMAAlpha)*cm.aggregatedMetrics.StreamingTokensPerSecondEWMA
			}
		}

		cm.aggregatedMetrics.StreamingSampleCount++

		return
	}

	latency := float64(requestLatencyMs)
	if cm.aggregatedMetrics.NonStreamingSampleCount == 0 {
		cm.aggregatedMetrics.NonStreamingLatencyEWMA = latency
	} else {
		cm.aggregatedMetrics.NonStreamingLatencyEWMA = latencyEWMAAlpha*latency + (1-latencyEWMAAlpha)*cm.aggregatedMetrics.NonStreamingLatencyEWMA
	}

	cm.aggregatedMetrics.NonStreamingSampleCount++
}

// recordFailure records a failed request to the channel metrics.
func (cm *channelMetrics) recordFailure(slot *timeSlotMetrics, perf *PerformanceRecord) {
	slot.FailureCount++
	cm.aggregatedMetrics.FailureCount++
	cm.aggregatedMetrics.LastFailureAt = &perf.EndTime

	// Increment consecutive failures
	cm.aggregatedMetrics.ConsecutiveFailures++
}

// getOrCreateTimeSlot gets or creates a time slot for the given timestamp.
func (cm *channelMetrics) getOrCreateTimeSlot(ts int64, endTime time.Time, windowSize int64) *timeSlotMetrics {
	if slot, ok := cm.window.Get(ts); ok {
		return slot
	}

	// Clean old entries to prevent memory leak
	if cm.window.Len() >= int(windowSize) {
		cm.cleanupExpiredSlots(endTime.Add(-time.Duration(windowSize) * time.Second))
	}

	slot := &timeSlotMetrics{
		timestamp:     ts,
		metricsRecord: metricsRecord{},
	}
	cm.window.Push(ts, slot)

	return slot
}

// RecordPerformance records performance metrics to in-memory cache.
// This function is not thread-safe.
func (svc *ChannelService) RecordPerformance(ctx context.Context, perf *PerformanceRecord) {
	if perf == nil || !perf.IsValid() {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			log.Error(ctx, "panic in record performance", log.Any("panic", r))
		}
	}()

	if perf.Success {
		svc.channelErrorCountsLock.Lock()
		delete(svc.channelErrorCounts, perf.ChannelID)
		svc.channelErrorCountsLock.Unlock()

		// Also clear API key error counts on success
		if perf.APIKey != "" {
			svc.apiKeyErrorCountsLock.Lock()

			if svc.apiKeyErrorCounts[perf.ChannelID] != nil {
				delete(svc.apiKeyErrorCounts[perf.ChannelID], perf.APIKey)
			}

			svc.apiKeyErrorCountsLock.Unlock()
		}
	} else if !perf.Canceled {
		policy := svc.SystemService.RetryPolicyOrDefault(ctx)

		if policy.AutoDisableChannel.Enabled {
			// Check API key error first if available.
			if perf.APIKey != "" {
				if svc.checkAndHandleAPIKeyError(ctx, perf, policy) {
					return
				}
			} else {
				if svc.checkAndHandleChannelError(ctx, perf, policy) {
					return
				}
			}
		}
	}

	// Protect the channel metrics object and its ring buffer for the whole
	// update. Readers clone under the same lock; selection increments also use
	// it, eliminating races between health recording and load balancing.
	svc.channelPerfMetricsLock.Lock()
	defer svc.channelPerfMetricsLock.Unlock()

	cm, exists := svc.channelPerfMetrics[perf.ChannelID]
	if !exists {
		cm = newChannelMetrics(perf.ChannelID)
		svc.channelPerfMetrics[perf.ChannelID] = cm
	}

	// Determine window size
	var windowSize int64 = defaultPerformanceWindowSize
	if svc.perfWindowSeconds > 0 {
		windowSize = svc.perfWindowSeconds
	}

	ts := perf.EndTime.Unix()

	// Get or create time slot for this second
	slot := cm.getOrCreateTimeSlot(ts, perf.EndTime, windowSize)

	// Record success or failure
	if perf.Success {
		cm.recordSuccess(slot, perf)
	} else if !perf.Canceled {
		cm.recordFailure(slot, perf)
	}

	if log.DebugEnabled(ctx) {
		keySuffix := ""
		if len(perf.APIKey) >= 4 {
			keySuffix = perf.APIKey[len(perf.APIKey)-4:]
		}
		log.Debug(ctx, "recorded performance metrics",
			log.Int("channel_id", perf.ChannelID),
			log.String("key_suffix", keySuffix), // Only log last 4 chars for security
			log.Bool("success", perf.Success),
			log.Any("error_code", perf.ResponseStatusCode),
		)
	}
}

// AsyncRecordPerformance records performance metrics to in-memory cache asynchronously.
func (svc *ChannelService) AsyncRecordPerformance(ctx context.Context, perr *PerformanceRecord) {
	if perr == nil {
		return
	}

	// The streaming pipeline can still observe trailing usage metadata after a
	// semantic terminal event. Queue an owned snapshot so the background metrics
	// worker never races with later writes to the per-attempt record.
	snapshot := *perr
	snapshot.FirstTokenTime = clonePerformanceTime(perr.FirstTokenTime)
	snapshot.ReasoningStartTime = clonePerformanceTime(perr.ReasoningStartTime)
	snapshot.ReasoningEndTime = clonePerformanceTime(perr.ReasoningEndTime)
	svc.perfCh <- &snapshot
}

func clonePerformanceTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}

	cloned := *value
	return &cloned
}

// cleanupExpiredSlots removes time slots older than the cutoff time.
// This is now O(k) where k is the number of items to remove, instead of O(n) for the entire map.
func (cm *channelMetrics) cleanupExpiredSlots(cutoff time.Time) {
	cutoffTs := cutoff.Unix()

	// Collect metrics to subtract before cleanup
	var metricsToRemove []*timeSlotMetrics

	cm.window.Range(func(ts int64, metrics *timeSlotMetrics) bool {
		if ts < cutoffTs {
			metricsToRemove = append(metricsToRemove, metrics)
			return true
		}
		// Since ringbuffer is ordered by timestamp, we can stop here
		return false
	})

	// Subtract removed metrics from aggregated metrics
	for _, metrics := range metricsToRemove {
		cm.aggregatedMetrics.SuccessCount -= metrics.SuccessCount
		cm.aggregatedMetrics.FailureCount -= metrics.FailureCount
	}

	// Cleanup old entries from ringbuffer
	cm.window.CleanupBefore(cutoffTs)
}

// GetChannelMetrics returns performance metrics for the channel.
// If in-memory metrics are not available (e.g., after restart), it falls back to database values.
func (svc *ChannelService) GetChannelMetrics(ctx context.Context, channelID int) (*AggregatedMetrics, error) {
	svc.channelPerfMetricsLock.RLock()
	defer svc.channelPerfMetricsLock.RUnlock()

	cm, exists := svc.channelPerfMetrics[channelID]

	if !exists {
		return &AggregatedMetrics{}, nil
	}

	// Return a full copy of the aggregated metrics to avoid concurrent modification
	// while preserving all load-balancing signals, including latency EWMA.
	return cm.aggregatedMetrics.Clone(), nil
}

// IncrementChannelSelection increments the request count for a channel at selection time.
// This is called when a channel is selected by the load balancer to ensure immediate
// impact on subsequent selections, preventing the same channel from being selected
// repeatedly during burst/concurrent requests.
func (svc *ChannelService) IncrementChannelSelection(channelID int) {
	svc.channelPerfMetricsLock.Lock()
	defer svc.channelPerfMetricsLock.Unlock()

	cm, exists := svc.channelPerfMetrics[channelID]
	if !exists {
		cm = newChannelMetrics(channelID)
		svc.channelPerfMetrics[channelID] = cm
	}

	oldCount := cm.aggregatedMetrics.RequestCount

	// Increment request count immediately to affect subsequent load balancing decisions
	cm.aggregatedMetrics.RequestCount++

	// Update last activity time to current time
	now := time.Now()
	if cm.aggregatedMetrics.LastSelectedAt == nil || cm.aggregatedMetrics.LastSelectedAt.Before(now) {
		cm.aggregatedMetrics.LastSelectedAt = &now
	}

	// Log debug message if enabled
	if log.DebugEnabled(context.Background()) {
		log.Debug(context.Background(), "IncrementChannelSelection: incremented request count",
			log.Int("channel_id", channelID),
			log.Int64("old_count", oldCount),
			log.Int64("new_count", cm.aggregatedMetrics.RequestCount),
		)
	}
}

func deriveErrorMessage(errorCode int) string {
	if text := http.StatusText(errorCode); text != "" {
		return text
	}

	return fmt.Sprintf("Error %d", errorCode)
}

// PerformanceRecord contains performance metrics collected during request processing.
type PerformanceRecord struct {
	ChannelID int
	APIKey    string // API key used for the request (sensitive, do not log full value)
	// Donated prevents transient upstream failures from permanently disabling a
	// student-contributed channel or one of its API keys. Donated channels still
	// participate in transient health tracking and recover automatically.
	Donated            bool
	StartTime          time.Time
	FirstTokenTime     *time.Time
	ReasoningStartTime *time.Time
	ReasoningEndTime   *time.Time
	EndTime            time.Time
	Stream             bool
	Success            bool
	Canceled           bool
	RequestCompleted   bool

	// If response status code is 0, it means the request is successful.
	ResponseStatusCode int
	CompletionTokens   int64
}

// Calculate calculates performance metrics from collected data.
// It enforces minimum latency to prevent extreme TPS calculations.
func (m *PerformanceRecord) Calculate() (firstTokenLatencyMs int64, requestLatencyMs int64, tokensPerSecond float64) {
	totalDuration := m.EndTime.Sub(m.StartTime)
	requestLatencyMs = totalDuration.Milliseconds()

	// Calculate first token latency
	if m.Stream && m.FirstTokenTime != nil {
		firstTokenLatency := m.FirstTokenTime.Sub(m.StartTime)
		firstTokenLatencyMs = firstTokenLatency.Milliseconds()
	}

	// Enforce minimum latency to prevent extreme TPS calculations
	requestLatencyMs = ClampLatency(requestLatencyMs)
	firstTokenLatencyMs = ClampLatency(firstTokenLatencyMs)

	if m.CompletionTokens > 0 {
		effectiveLatencyMs := requestLatencyMs
		if m.Stream && m.FirstTokenTime != nil {
			effectiveLatencyMs = requestLatencyMs - firstTokenLatencyMs
			effectiveLatencyMs = ClampLatency(effectiveLatencyMs)
		}

		tokensPerSecond = float64(m.CompletionTokens) / (float64(effectiveLatencyMs) / 1000.0)
	}

	return firstTokenLatencyMs, requestLatencyMs, tokensPerSecond
}

// CalculateReasoningDurationMs calculates the reasoning duration.
func (m *PerformanceRecord) CalculateReasoningDurationMs() int64 {
	if m.ReasoningStartTime == nil || m.ReasoningEndTime == nil {
		return 0
	}
	duration := m.ReasoningEndTime.Sub(*m.ReasoningStartTime)
	return duration.Milliseconds()
}

// MarkSuccess marks the request as completed.
func (m *PerformanceRecord) MarkSuccess() {
	m.Success = true
	m.RequestCompleted = true
	m.EndTime = time.Now()
}

// MarkFirstToken marks the first token time.
func (m *PerformanceRecord) MarkFirstToken() {
	if m.FirstTokenTime == nil {
		now := time.Now()
		m.FirstTokenTime = &now
	}
}

// MarkReasoningStart marks the reasoning start time.
func (m *PerformanceRecord) MarkReasoningStart() {
	if m.ReasoningStartTime == nil {
		now := time.Now()
		m.ReasoningStartTime = &now
	}
}

// MarkReasoningEnd marks the reasoning end time.
func (m *PerformanceRecord) MarkReasoningEnd() {
	if m.ReasoningEndTime == nil {
		now := time.Now()
		m.ReasoningEndTime = &now
	}
}

// MarkFailed marks the request as failed.
func (m *PerformanceRecord) MarkFailed(errorCode int) {
	m.Success = false
	m.ResponseStatusCode = errorCode
	m.RequestCompleted = true
	m.EndTime = time.Now()
}

// MarkCanceled marks the request as canceled by context.
func (m *PerformanceRecord) MarkCanceled() {
	m.Success = false
	m.Canceled = true
	m.RequestCompleted = true
	m.EndTime = time.Now()
}

// IsValid checks if metrics are valid for recording.
func (m *PerformanceRecord) IsValid() bool {
	return m.ChannelID > 0 && m.RequestCompleted
}
