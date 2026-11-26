package toroid

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"time"
)

const (
	metadataTTL   = 10 * time.Minute
	metadataRetry = 15 * time.Second
)

type metadataKey struct {
	base, model string
	credential  [sha256.Size]byte
}

type metadataEntry[T any] struct {
	ready   chan struct{}
	value   T
	expires time.Time
}

func (e *metadataEntry[T]) wait(ctx context.Context) (value T) {
	if e != nil {
		// A sibling lookup may have consumed the whole wait budget. Still
		// honor this result when it is already cached.
		select {
		case <-e.ready:
			return e.value
		default:
		}
		select {
		case <-e.ready:
			return e.value
		case <-ctx.Done():
		}
	}
	return value
}

type metadataCache[T any] struct {
	mu      sync.Mutex
	entries map[metadataKey]*metadataEntry[T]
}

// All callers for a key share one bounded gateway probe. A canceled chat
// stops waiting immediately, without canceling another chat's discovery.
func (c *metadataCache[T]) start(ctx context.Context, key metadataKey, fetch func(context.Context) (T, bool)) *metadataEntry[T] {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if e := c.entries[key]; e != nil && (e.expires.IsZero() || now.Before(e.expires)) {
		return e
	}
	if c.entries == nil {
		c.entries = make(map[metadataKey]*metadataEntry[T])
	}
	for k, e := range c.entries {
		if !e.expires.IsZero() && !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	e := &metadataEntry[T]{ready: make(chan struct{})}
	c.entries[key] = e
	go func() {
		value, ok := fetch(context.WithoutCancel(ctx))
		lifetime := metadataTTL
		if !ok {
			lifetime = metadataRetry
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		e.value = value
		e.expires = time.Now().Add(lifetime)
		close(e.ready)
	}()
	return e
}

var kernelLimits metadataCache[modelLimits]
var kernelCatalog metadataCache[[]GatewayModel]

func startCatalog(ctx context.Context, base, apiKey string) *metadataEntry[[]GatewayModel] {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil
	}
	key := metadataKey{base: base, credential: sha256.Sum256([]byte(apiKey))}
	return kernelCatalog.start(ctx, key, func(ctx context.Context) ([]GatewayModel, bool) {
		models, err := FetchGatewayModels(ctx, nil, base, apiKey)
		return models, err == nil
	})
}

// CachedGatewayModels shares the configured gateway catalog across chat requests.
func CachedGatewayModels(ctx context.Context, base, apiKey string) []GatewayModel {
	return startCatalog(ctx, base, apiKey).wait(ctx)
}

func startKernelMetadata(ctx context.Context, cfg Config, base, apiKey string) (*metadataEntry[modelLimits], *metadataEntry[[]GatewayModel]) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || ctx.Err() != nil {
		return nil, nil
	}
	key := metadataKey{base: base, credential: sha256.Sum256([]byte(apiKey))}
	var catalog *metadataEntry[[]GatewayModel]
	if cfg.IncludeSubagentTools && !strings.HasPrefix(cfg.Model, "openai/") {
		catalog = startCatalog(ctx, base, apiKey)
	}
	key.model = strings.TrimPrefix(cfg.Model, "llmgateway/")
	limits := kernelLimits.start(ctx, key, func(ctx context.Context) (modelLimits, bool) {
		return fetchModelLimits(ctx, base, apiKey, cfg.Model)
	})
	return limits, catalog
}

// WarmModelMetadata starts the same gateway lookups as NewKernel in the
// background. Hosts can call this before the first prompt arrives.
func WarmModelMetadata(cfg Config) {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	base, key := resolveProviderBaseURL(cfg)
	startKernelMetadata(context.Background(), cfg, base, key)
}
