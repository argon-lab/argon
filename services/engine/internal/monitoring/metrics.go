package monitoring

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/attribute"
)

// Metrics holds all the metrics instruments
type Metrics struct {
	// Branch operation metrics
	BranchOperations   metric.Int64Counter
	BranchOperationDuration metric.Float64Histogram
	ActiveBranches     metric.Int64UpDownCounter
	
	// Data operation metrics
	DataOperations     metric.Int64Counter
	DataThroughput     metric.Float64Counter
	CompressionRatio   metric.Float64Gauge
	
	// Worker metrics
	WorkerPoolSize     metric.Int64UpDownCounter
	QueuedJobs         metric.Int64UpDownCounter
	ProcessedJobs      metric.Int64Counter
	JobProcessingTime  metric.Float64Histogram
	
	// Storage metrics
	StorageUsage       metric.Float64Gauge
	S3Operations       metric.Int64Counter
	S3OperationDuration metric.Float64Histogram
	
	// MongoDB metrics
	MongoOperations    metric.Int64Counter
	MongoConnections   metric.Int64UpDownCounter
	ChangeStreamEvents metric.Int64Counter
	
	// System metrics
	MemoryUsage        metric.Float64Gauge
	CPUUsage          metric.Float64Gauge
	ErrorCount        metric.Int64Counter
}

var GlobalMetrics *Metrics

// InitMetrics initializes OpenTelemetry metrics with Prometheus exporter
func InitMetrics() error {
	// Create Prometheus exporter
	exporter, err := prometheus.New()
	if err != nil {
		return fmt.Errorf("failed to create prometheus exporter: %w", err)
	}

	// Create metric provider
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
	)
	otel.SetMeterProvider(provider)

	// Create meter
	meter := otel.Meter("argon-engine")

	// Initialize metrics
	GlobalMetrics = &Metrics{}

	// Branch operation metrics
	if GlobalMetrics.BranchOperations, err = meter.Int64Counter(
		"argon_branch_operations_total",
		metric.WithDescription("Total number of branch operations"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	if GlobalMetrics.BranchOperationDuration, err = meter.Float64Histogram(
		"argon_branch_operation_duration_seconds",
		metric.WithDescription("Duration of branch operations"),
		metric.WithUnit("s"),
	); err != nil {
		return err
	}

	if GlobalMetrics.ActiveBranches, err = meter.Int64UpDownCounter(
		"argon_active_branches",
		metric.WithDescription("Number of active branches"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	// Data operation metrics
	if GlobalMetrics.DataOperations, err = meter.Int64Counter(
		"argon_data_operations_total",
		metric.WithDescription("Total number of data operations"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	if GlobalMetrics.DataThroughput, err = meter.Float64Counter(
		"argon_data_throughput_ops_per_second",
		metric.WithDescription("Data operations throughput"),
		metric.WithUnit("ops/s"),
	); err != nil {
		return err
	}

	if GlobalMetrics.CompressionRatio, err = meter.Float64Gauge(
		"argon_compression_ratio",
		metric.WithDescription("Data compression ratio"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	// Worker metrics
	if GlobalMetrics.WorkerPoolSize, err = meter.Int64UpDownCounter(
		"argon_worker_pool_size",
		metric.WithDescription("Current worker pool size"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	if GlobalMetrics.QueuedJobs, err = meter.Int64UpDownCounter(
		"argon_queued_jobs",
		metric.WithDescription("Number of jobs in queue"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	if GlobalMetrics.ProcessedJobs, err = meter.Int64Counter(
		"argon_processed_jobs_total",
		metric.WithDescription("Total number of processed jobs"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	if GlobalMetrics.JobProcessingTime, err = meter.Float64Histogram(
		"argon_job_processing_duration_seconds",
		metric.WithDescription("Job processing duration"),
		metric.WithUnit("s"),
	); err != nil {
		return err
	}

	// Storage metrics
	if GlobalMetrics.StorageUsage, err = meter.Float64Gauge(
		"argon_storage_usage_bytes",
		metric.WithDescription("Storage usage in bytes"),
		metric.WithUnit("By"),
	); err != nil {
		return err
	}

	if GlobalMetrics.S3Operations, err = meter.Int64Counter(
		"argon_s3_operations_total",
		metric.WithDescription("Total number of S3 operations"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	if GlobalMetrics.S3OperationDuration, err = meter.Float64Histogram(
		"argon_s3_operation_duration_seconds",
		metric.WithDescription("S3 operation duration"),
		metric.WithUnit("s"),
	); err != nil {
		return err
	}

	// MongoDB metrics
	if GlobalMetrics.MongoOperations, err = meter.Int64Counter(
		"argon_mongo_operations_total",
		metric.WithDescription("Total number of MongoDB operations"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	if GlobalMetrics.MongoConnections, err = meter.Int64UpDownCounter(
		"argon_mongo_connections",
		metric.WithDescription("Number of active MongoDB connections"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	if GlobalMetrics.ChangeStreamEvents, err = meter.Int64Counter(
		"argon_change_stream_events_total",
		metric.WithDescription("Total number of change stream events"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	// System metrics
	if GlobalMetrics.MemoryUsage, err = meter.Float64Gauge(
		"argon_memory_usage_bytes",
		metric.WithDescription("Memory usage in bytes"),
		metric.WithUnit("By"),
	); err != nil {
		return err
	}

	if GlobalMetrics.CPUUsage, err = meter.Float64Gauge(
		"argon_cpu_usage_percent",
		metric.WithDescription("CPU usage percentage"),
		metric.WithUnit("%"),
	); err != nil {
		return err
	}

	if GlobalMetrics.ErrorCount, err = meter.Int64Counter(
		"argon_errors_total",
		metric.WithDescription("Total number of errors"),
		metric.WithUnit("1"),
	); err != nil {
		return err
	}

	log.Println("Metrics initialized successfully")
	return nil
}

// StartMetricsServer starts the Prometheus metrics server
func StartMetricsServer(port string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	
	// Add health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	log.Printf("Metrics server starting on port %s", port)
	if err := server.ListenAndServe(); err != nil {
		log.Printf("Metrics server error: %v", err)
	}
}

// RecordBranchOperation records metrics for branch operations
func RecordBranchOperation(ctx context.Context, operation string, duration time.Duration, success bool) {
	if GlobalMetrics == nil {
		return
	}

	attrs := metric.WithAttributes(
		attribute.String("operation", operation),
		attribute.Bool("success", success),
	)

	GlobalMetrics.BranchOperations.Add(ctx, 1, attrs)
	GlobalMetrics.BranchOperationDuration.Record(ctx, duration.Seconds(), attrs)

	if !success {
		GlobalMetrics.ErrorCount.Add(ctx, 1, metric.WithAttributes(
			attribute.String("type", "branch_operation"),
		))
	}
}

// RecordDataOperation records metrics for data operations
func RecordDataOperation(ctx context.Context, operationType string, count int64) {
	if GlobalMetrics == nil {
		return
	}

	attrs := metric.WithAttributes(
		attribute.String("type", operationType),
	)

	GlobalMetrics.DataOperations.Add(ctx, count, attrs)
}

// UpdateThroughput updates the data throughput metric
func UpdateThroughput(ctx context.Context, opsPerSecond float64) {
	if GlobalMetrics == nil {
		return
	}

	GlobalMetrics.DataThroughput.Add(ctx, opsPerSecond)
}

// UpdateCompressionRatio updates the compression ratio metric
func UpdateCompressionRatio(ctx context.Context, ratio float64) {
	if GlobalMetrics == nil {
		return
	}

	GlobalMetrics.CompressionRatio.Record(ctx, ratio)
}

// RecordWorkerPoolChange records changes in worker pool size
func RecordWorkerPoolChange(ctx context.Context, delta int64) {
	if GlobalMetrics == nil {
		return
	}

	GlobalMetrics.WorkerPoolSize.Add(ctx, delta)
}

// RecordJobMetrics records job processing metrics
func RecordJobMetrics(ctx context.Context, jobType string, duration time.Duration, success bool) {
	if GlobalMetrics == nil {
		return
	}

	attrs := metric.WithAttributes(
		attribute.String("job_type", jobType),
		attribute.Bool("success", success),
	)

	GlobalMetrics.ProcessedJobs.Add(ctx, 1, attrs)
	GlobalMetrics.JobProcessingTime.Record(ctx, duration.Seconds(), attrs)

	if !success {
		GlobalMetrics.ErrorCount.Add(ctx, 1, metric.WithAttributes(
			attribute.String("type", "job_processing"),
		))
	}
}

// UpdateQueueSize updates the job queue size
func UpdateQueueSize(ctx context.Context, size int64) {
	if GlobalMetrics == nil {
		return
	}

	GlobalMetrics.QueuedJobs.Add(ctx, size)
}

// RecordS3Operation records S3 operation metrics
func RecordS3Operation(ctx context.Context, operation string, duration time.Duration, success bool) {
	if GlobalMetrics == nil {
		return
	}

	attrs := metric.WithAttributes(
		attribute.String("operation", operation),
		attribute.Bool("success", success),
	)

	GlobalMetrics.S3Operations.Add(ctx, 1, attrs)
	GlobalMetrics.S3OperationDuration.Record(ctx, duration.Seconds(), attrs)

	if !success {
		GlobalMetrics.ErrorCount.Add(ctx, 1, metric.WithAttributes(
			attribute.String("type", "s3_operation"),
		))
	}
}

// RecordMongoOperation records MongoDB operation metrics
func RecordMongoOperation(ctx context.Context, operation string, count int64) {
	if GlobalMetrics == nil {
		return
	}

	attrs := metric.WithAttributes(
		attribute.String("operation", operation),
	)

	GlobalMetrics.MongoOperations.Add(ctx, count, attrs)
}

// RecordChangeStreamEvent records change stream events
func RecordChangeStreamEvent(ctx context.Context, eventType string) {
	if GlobalMetrics == nil {
		return
	}

	attrs := metric.WithAttributes(
		attribute.String("event_type", eventType),
	)

	GlobalMetrics.ChangeStreamEvents.Add(ctx, 1, attrs)
}

// UpdateSystemMetrics updates system resource metrics
func UpdateSystemMetrics(ctx context.Context, memoryBytes, cpuPercent float64) {
	if GlobalMetrics == nil {
		return
	}

	GlobalMetrics.MemoryUsage.Record(ctx, memoryBytes)
	GlobalMetrics.CPUUsage.Record(ctx, cpuPercent)
}