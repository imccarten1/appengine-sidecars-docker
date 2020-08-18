package nginxreceiver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	metricspb "github.com/census-instrumentation/opencensus-proto/gen-go/metrics/v1"

	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumerdata"
	"go.opentelemetry.io/collector/consumer/pdatautil"
	"go.uber.org/zap"

	"github.com/googlecloudplatform/appengine-sidecars-docker/opentelemetry_collector/receiver/metricgenerator"
)

type NginxStatsCollector struct {
	consumer consumer.MetricsConsumer

	now       func() time.Time
	startTime time.Time
	done      chan struct{}
	logger    *zap.Logger
	getStatus func(string) (resp *http.Response, err error)

	exportInterval time.Duration
	statsUrl       string
}

type LatencyStats struct {
	RequestCount int64   `json:"request_count"`
	LatencySum   int64   `json:"latency_sum"`
	SumSquares   int64   `json:"sum_squares"`
	Distribution []int64 `json:"distribution"`
}

type NginxStats struct {
	RequestLatency      LatencyStats `json:"request_latency"`
	UpstreamLatency     LatencyStats `json:"upstream_latency"`
	WebsocketLatency    LatencyStats `json:"websocket_latency"`
	LatencyBucketBounds []float64    `json:"latency_bucket_bounds"`
}

const (
	defaultExportInterval = time.Minute
)

func getHttp(url string) (resp *http.Response, err error) {
	return http.Get(url)
}

func NewNginxStatsCollector(exportInterval time.Duration, url string, logger *zap.Logger, consumer consumer.MetricsConsumer) *NginxStatsCollector {
	if exportInterval <= 0 {
		exportInterval = defaultExportInterval
	}

	collector := &NginxStatsCollector{
		consumer:       consumer,
		now:            time.Now,
		done:           make(chan struct{}),
		logger:         logger,
		exportInterval: exportInterval,
		statsUrl:       url,
		getStatus:      getHttp,
	}

	return collector
}

func (collector *NginxStatsCollector) StartCollection() {
	collector.startTime = collector.now()

	go func() {
		ticker := time.NewTicker(collector.exportInterval)
		for {
			select {
			case <-ticker.C:
				collector.scrapeAndExport()
			case <-collector.done:
				return
			}
		}
	}()
}

func (collector *NginxStatsCollector) StopCollection() {
	close(collector.done)
}

func (collector *NginxStatsCollector) scrapeNginxStats() (*NginxStats, error) {
	resp, err := collector.getStatus(collector.statsUrl)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Error getting nginx stats. status code: %d", resp.StatusCode)
	}

	defer resp.Body.Close()
	statsJson, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var stats NginxStats
	if err = json.Unmarshal([]byte(statsJson), &stats); err != nil {
		return nil, err
	}

	return &stats, nil
}

func (collector *NginxStatsCollector) appendDistributionMetric(
	stats *LatencyStats,
	bucketOptions *metricspb.DistributionValue_BucketOptions,
	metrics []*metricspb.Metric,
	descriptor *metricspb.MetricDescriptor) []*metricspb.Metric {

	sumSquaredDeviation := metricgenerator.GetSumOfSquaredDeviationFromIntDist(
		stats.LatencySum, stats.SumSquares, stats.RequestCount)
	timeseries := metricgenerator.MakeDistributionTimeSeries(
		stats.Distribution,
		float64(stats.LatencySum),
		sumSquaredDeviation,
		stats.RequestCount,
		collector.startTime,
		collector.now(),
		bucketOptions,
		[]*metricspb.LabelValue{},
	)
	return append(metrics, &metricspb.Metric{
		MetricDescriptor: descriptor,
		Timeseries:       []*metricspb.TimeSeries{timeseries},
	},
	)
}

func (stats *LatencyStats) checkConsistency(bounds []float64) error {
	if len(bounds)+1 != len(stats.Distribution) {
		return errors.New("The length of the latency distribution and distribution bucket boundaries do not match")
	}

	if stats.RequestCount < 0 {
		return errors.New("The request count is less than 0")
	}

	if stats.SumSquares < 0 {
		return errors.New("The sum of squared latencies is less than 0")
	}

	if stats.LatencySum < 0 {
		return errors.New("The sum of latencies is less than 0")
	}

	for _, count := range stats.Distribution {
		if count < 0 {
			return errors.New("One of the latency distribution counts is less than 0")
		}
	}
	return nil
}

func (collector *NginxStatsCollector) scrapeAndExport() {
	metrics := make([]*metricspb.Metric, 0, 3)

	stats, err := collector.scrapeNginxStats()
	if err != nil {
		collector.logger.Error("Could not read nginx stats", zap.Error(err))
	} else {
		bucketOptions := metricgenerator.FormatBucketOptions(stats.LatencyBucketBounds)

		if err = stats.RequestLatency.checkConsistency(stats.LatencyBucketBounds); err != nil {
			collector.logger.Error("Invalid value received for RequestLatency", zap.Error(err))
		} else {
			metrics = collector.appendDistributionMetric(&stats.RequestLatency, bucketOptions, metrics, requestLatencyMetric)
		}
		if err = stats.WebsocketLatency.checkConsistency(stats.LatencyBucketBounds); err != nil {
			collector.logger.Error("Invalid value received for WebsocketLatency", zap.Error(err))
		} else {
			metrics = collector.appendDistributionMetric(&stats.WebsocketLatency, bucketOptions, metrics, websocketLatencyMetric)
		}

		if err = stats.UpstreamLatency.checkConsistency(stats.LatencyBucketBounds); err != nil {
			collector.logger.Error("Invalid value received for UpstreamLatency", zap.Error(err))
		} else {
			metrics = collector.appendDistributionMetric(&stats.UpstreamLatency, bucketOptions, metrics, upstreamLatencyMetric)
		}
	}

	ctx := context.Background()
	md := consumerdata.MetricsData{Metrics: metrics}
	collector.consumer.ConsumeMetrics(ctx, pdatautil.MetricsFromMetricsData([]consumerdata.MetricsData{md}))
}
