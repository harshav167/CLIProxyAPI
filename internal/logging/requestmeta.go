package logging

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
)

type endpointKey struct{}
type cacheIdentityKey struct{}
type responseStatusKey struct{}
type responseHeadersKey struct{}
type requestSummaryKey struct{}

type CacheIdentity struct {
	ConversationID string
	PromptCacheKey string
}

type RequestSummary struct {
	Model               string
	RequestedModel      string
	EndpointFamily      string
	Provider            string
	Status              int
	RequestBytes        int64
	ResponseBytes       int64
	CacheControlSummary string
	RequestBody         string
	ResponseBody        string
}

type responseStatusHolder struct {
	status atomic.Int32
}

type responseHeadersHolder struct {
	mu      sync.RWMutex
	headers http.Header
}

type requestSummaryHolder struct {
	mu      sync.RWMutex
	summary RequestSummary
}

func WithEndpoint(ctx context.Context, endpoint string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, endpointKey{}, endpoint)
}

func GetEndpoint(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if endpoint, ok := ctx.Value(endpointKey{}).(string); ok {
		return endpoint
	}
	return ""
}

func WithCacheIdentity(ctx context.Context, conversationID, promptCacheKey string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, cacheIdentityKey{}, CacheIdentity{
		ConversationID: conversationID,
		PromptCacheKey: promptCacheKey,
	})
}

func GetCacheIdentity(ctx context.Context) CacheIdentity {
	if ctx == nil {
		return CacheIdentity{}
	}
	if identity, ok := ctx.Value(cacheIdentityKey{}).(CacheIdentity); ok {
		return identity
	}
	return CacheIdentity{}
}

func WithResponseStatusHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, responseStatusKey{}, &responseStatusHolder{})
}

func WithResponseHeadersHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, responseHeadersKey{}, &responseHeadersHolder{})
}

func WithRequestSummaryHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(requestSummaryKey{}).(*requestSummaryHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, requestSummaryKey{}, &requestSummaryHolder{})
}

func SetResponseStatus(ctx context.Context, status int) {
	if ctx == nil || status <= 0 {
		return
	}
	holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder)
	if !ok || holder == nil {
		return
	}
	holder.status.Store(int32(status))
}

func SetResponseHeaders(ctx context.Context, headers http.Header) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	holder.headers = cloneHTTPHeader(headers)
}

func GetResponseStatus(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	holder, ok := ctx.Value(responseStatusKey{}).(*responseStatusHolder)
	if !ok || holder == nil {
		return 0
	}
	return int(holder.status.Load())
}

func GetResponseHeaders(ctx context.Context) http.Header {
	if ctx == nil {
		return nil
	}
	holder, ok := ctx.Value(responseHeadersKey{}).(*responseHeadersHolder)
	if !ok || holder == nil {
		return nil
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return cloneHTTPHeader(holder.headers)
}

func SetRequestSummary(ctx context.Context, summary RequestSummary) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(requestSummaryKey{}).(*requestSummaryHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	holder.summary = summary
}

func UpdateRequestSummary(ctx context.Context, update func(*RequestSummary)) {
	if ctx == nil || update == nil {
		return
	}
	holder, ok := ctx.Value(requestSummaryKey{}).(*requestSummaryHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	update(&holder.summary)
}

func GetRequestSummary(ctx context.Context) RequestSummary {
	if ctx == nil {
		return RequestSummary{}
	}
	holder, ok := ctx.Value(requestSummaryKey{}).(*requestSummaryHolder)
	if !ok || holder == nil {
		return RequestSummary{}
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return holder.summary
}

func cloneHTTPHeader(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}
