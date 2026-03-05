package appinit

import (
	"context"
	"fmt"

	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.6.1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// InitLogger builds a zap.Logger for the given debug mode.
// In debug mode, a development logger is created and the appMetadata fields are
// logged as an Info message.  In production mode, the fields are attached to
// every subsequent log entry via logger.With.
//
// The caller owns the returned logger and is responsible for calling Sync.
func InitLogger(debug bool, appMetadata []zap.Field) (*zap.Logger, error) {
	var (
		logger *zap.Logger
		err    error
	)

	if debug {
		logger, err = logutil.ZapDevelopmentConfig().Build()
		if err != nil {
			return nil, err
		}
		logger.Info("Debug mode enabled", appMetadata...)
	} else {
		logger, err = logutil.ZapProductionConfig().Build()
		if err != nil {
			return nil, err
		}
		logger = logger.With(appMetadata...)
	}

	// Best-effort sync; errors here are non-fatal.
	defer func() { _ = logger.Sync() }()

	return logger, nil
}

// InitOpenTelemetry configures a global TracerProvider backed by an OTLP/gRPC
// exporter.  If collectorURL is empty, traces are still collected in-process
// but not exported anywhere (useful for local development).
//
// The returned function must be called on shutdown to flush and close the
// exporter cleanly.
func InitOpenTelemetry(appName, version, buildTime, commitHash, environment, collectorURL string) (func(context.Context) error, error) {
	ctx := context.Background()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(appName),
			semconv.ServiceVersionKey.String(version),
			semconv.ServiceNamespaceKey.String("sdc"),
			semconv.DeploymentEnvironmentKey.String(environment),
			attribute.String("service.commit_hash", commitHash),
			attribute.String("service.build_time", buildTime),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTel resource: %w", err)
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	}

	if collectorURL != "" {
		conn, err := grpc.NewClient(collectorURL, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, fmt.Errorf("failed to create gRPC connection to OTel collector: %w", err)
		}

		exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
		if err != nil {
			return nil, fmt.Errorf("failed to create OTel trace exporter: %w", err)
		}

		opts = append(opts, sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(exporter)))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}
