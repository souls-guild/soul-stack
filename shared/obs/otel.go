package obs

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// OTelConfig — параметры bootstrap-а OTel-провайдера. Компонент-агностичен:
// keeper и soul передают своё ServiceName ("keeper" / "soul") и доменную
// идентичность инстанса через ResourceAttrs (keeper — soulstack.kid,
// soul — soulstack.sid; ADR-024 §3). Сам провайдер про эти ключи не знает.
type OTelConfig struct {
	// Enabled — включает экспорт. При false [SetupOTel] возвращает no-op
	// провайдер (не nil): main не ветвится, defer-цепочка единообразна.
	Enabled bool

	// Endpoint — адрес OTLP-коллектора (host:port). Используется только
	// при Enabled; при пустом Endpoint exporter не поднимается даже с
	// Enabled (трейсы пишутся в no-op span-processor).
	Endpoint string

	// ServiceName — стандартный OTel semconv-атрибут service.name
	// ("keeper" | "soul" по словарю Soul Stack).
	ServiceName string

	// ResourceAttrs — кастомные resource-attributes источника
	// (soulstack.kid / soulstack.sid и пр.). Накладываются поверх
	// service.name; пустая map допустима.
	ResourceAttrs map[string]string
}

// OTelProvider — дескриптор поднятого OTel-стека. Держит TracerProvider и
// (опционально) OTLP-exporter для корректного flush+close на shutdown-е.
//
// В Slice 0 — только TracerProvider; инструментаций (span-ов) ещё нет
// (Slice 2). Provider регистрируется как глобальный [otel.SetTracerProvider]
// + propagator, чтобы будущие инструментации брали его без явной передачи.
type OTelProvider struct {
	tp *sdktrace.TracerProvider
}

// SetupOTel поднимает OTel-провайдер по конфигу. Всегда возвращает
// не-nil [OTelProvider] (даже при Enabled=false) — main вешает Shutdown в
// defer без проверки на nil.
//
// Вызывать ОДИН РАЗ за процесс. SetupOTel ставит глобальный
// [otel.SetTracerProvider] + propagator — повторный вызов без [Shutdown]
// предыдущего провайдера течёт (старый TracerProvider с его batch-export
// pipeline остаётся жив, но недостижим). otel.*-блок конфига —
// restart-required: hot-reload его НЕ перечитывает (менять endpoint/enabled
// на лету = пересоздавать провайдер с risk-ом потери in-flight span-ов).
//
// Resource собирается из semconv.ServiceName(cfg.ServiceName) +
// кастомных ResourceAttrs, поверх [resource.Default] (host/sdk-атрибуты).
// Сэмплер — ParentBased(AlwaysSample): корневые span-ы сэмплируются
// всегда, дочерние наследуют решение родителя (config-поле sampler не
// вводим — отложено).
//
// При Enabled+Endpoint поднимается OTLP-gRPC batch-exporter; иначе
// TracerProvider работает без exporter-а (span-ы никуда не уходят, но
// API не ломается — удобно для dev без коллектора).
func SetupOTel(ctx context.Context, cfg OTelConfig) (*OTelProvider, error) {
	res, err := buildResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	}

	if cfg.Enabled && cfg.Endpoint != "" {
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
			// Insecure: dev-коллектор поднимается локально через
			// docker-compose без TLS (project_local_dev_docker). TLS к
			// коллектору — отдельная задача при появлении prod-инсталляции.
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			return nil, fmt.Errorf("obs: build OTLP trace-exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagator())

	return &OTelProvider{tp: tp}, nil
}

// Shutdown флашит pending-span-ы и закрывает exporter. Безопасен на любом
// возвращённом провайдере (включая Enabled=false — Shutdown no-op
// TracerProvider просто завершается). Вешается в defer-цепочку main с
// timeout (Slice 1).
func (p *OTelProvider) Shutdown(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	if err := p.tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("obs: shutdown OTel tracer-provider: %w", err)
	}
	return nil
}

// buildResource собирает OTel-resource из service.name + кастомных
// ResourceAttrs (soulstack.kid / soulstack.sid), поверх resource.Default()
// (host/process/sdk-атрибуты). Вынесено из SetupOTel для прямого теста
// resource-слоя без подъёма TracerProvider-а.
//
// resource.New без WithSchemaURL: наш слой остаётся schema-less, чтобы
// merge с resource.Default() (несёт schema-URL версии semconv из SDK) не
// падал на conflicting Schema URL.
func buildResource(ctx context.Context, cfg OTelConfig) (*resource.Resource, error) {
	attrs := make([]attribute.KeyValue, 0, len(cfg.ResourceAttrs)+1)
	attrs = append(attrs, semconv.ServiceName(cfg.ServiceName))
	for k, v := range cfg.ResourceAttrs {
		attrs = append(attrs, attribute.String(k, v))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		return nil, fmt.Errorf("obs: build OTel resource: %w", err)
	}
	merged, err := resource.Merge(resource.Default(), res)
	if err != nil {
		return nil, fmt.Errorf("obs: merge OTel resource: %w", err)
	}
	return merged, nil
}

// propagator — W3C TraceContext + Baggage. TraceContext несёт trace-id /
// span-id через gRPC-метаданные EventStream-а (сквозной трейс оператор →
// Keeper → Soul, ADR-024 §1.2); Baggage — для domain-context-проброса.
// Глобальный propagator ставится в [SetupOTel], чтобы инструментации
// (Slice 2) inject/extract без явной передачи.
func propagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}
