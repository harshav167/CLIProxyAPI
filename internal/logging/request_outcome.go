package logging

import (
	"context"
	"strings"
	"sync"
)

type requestOutcomeKey struct{}

type RequestOutcome struct {
	Failed          bool
	ErrorStatus     int
	ErrorMessage    string
	Provider        string
	Model           string
	RequestedModel  string
	AuthFingerprint string
	Client          string
	// ClientCanceled is a downstream disconnect after the upstream request
	// started. It is tracked separately from proxy or provider failures.
	ClientCanceled bool
}

type requestOutcomeHolder struct {
	mu      sync.RWMutex
	outcome RequestOutcome
}

func WithRequestOutcomeHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(requestOutcomeKey{}).(*requestOutcomeHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, requestOutcomeKey{}, &requestOutcomeHolder{})
}

func SetRequestOutcome(ctx context.Context, failed bool, errorStatus int, errorMessage string) {
	if ctx == nil || !failed {
		return
	}
	holder, ok := ctx.Value(requestOutcomeKey{}).(*requestOutcomeHolder)
	if !ok || holder == nil {
		return
	}
	errorMessage = strings.TrimSpace(errorMessage)
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if errorStatus > holder.outcome.ErrorStatus {
		holder.outcome.ErrorStatus = errorStatus
	}
	if errorMessage != "" {
		holder.outcome.ErrorMessage = errorMessage
	}
	holder.outcome.Failed = true
}

// SetRequestIdentity records upstream identity before usage publishing, so
// early failures still carry provider, model, and credential context.
func SetRequestIdentity(ctx context.Context, provider, model, requestedModel, authFingerprint string) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(requestOutcomeKey{}).(*requestOutcomeHolder)
	if !ok || holder == nil {
		return
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	requestedModel = strings.TrimSpace(requestedModel)
	authFingerprint = strings.TrimSpace(authFingerprint)
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if provider != "" && holder.outcome.Provider == "" {
		holder.outcome.Provider = provider
	}
	if model != "" && holder.outcome.Model == "" {
		holder.outcome.Model = model
	}
	if requestedModel != "" && holder.outcome.RequestedModel == "" {
		holder.outcome.RequestedModel = requestedModel
	}
	if authFingerprint != "" && holder.outcome.AuthFingerprint == "" {
		holder.outcome.AuthFingerprint = authFingerprint
	}
}

// SetRequestClientCanceled marks a downstream disconnect without turning it
// into an upstream failure.
func SetRequestClientCanceled(ctx context.Context) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(requestOutcomeKey{}).(*requestOutcomeHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	holder.outcome.ClientCanceled = true
}

// SetRequestClient records the caller identity from X-Client or User-Agent.
func SetRequestClient(ctx context.Context, client string) {
	if ctx == nil {
		return
	}
	holder, ok := ctx.Value(requestOutcomeKey{}).(*requestOutcomeHolder)
	if !ok || holder == nil {
		return
	}
	client = strings.TrimSpace(client)
	if client == "" {
		return
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if holder.outcome.Client == "" {
		holder.outcome.Client = client
	}
}

func GetRequestOutcome(ctx context.Context) RequestOutcome {
	if ctx == nil {
		return RequestOutcome{}
	}
	holder, ok := ctx.Value(requestOutcomeKey{}).(*requestOutcomeHolder)
	if !ok || holder == nil {
		return RequestOutcome{}
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return holder.outcome
}
