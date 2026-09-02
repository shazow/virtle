// Package sessionbridge carries CLI-only lifecycle hooks between the public
// QEMU backend and its foreground session package without adding them to the
// supported backend API.
package sessionbridge

import (
	"context"
	"sync"
)

type contextKey struct{}

// Hooks are installed by the QEMU backend after a machine has started.
type Hooks struct {
	SuspendRequests      func() <-chan struct{}
	HandleSuspendRequest func(context.Context) error
	Suspend              func(context.Context) error
	CommitResume         func() error
}

// Bridge owns the hooks for one foreground session.
type Bridge struct {
	mu    sync.RWMutex
	hooks Hooks
}

// Suspend handles a local foreground suspend signal.
func (b *Bridge) Suspend(ctx context.Context) error {
	b.mu.RLock()
	suspend := b.hooks.Suspend
	b.mu.RUnlock()
	if suspend == nil {
		return nil
	}
	return suspend(ctx)
}

// WithContext marks ctx as a foreground session start.
func WithContext(ctx context.Context, bridge *Bridge) context.Context {
	return context.WithValue(ctx, contextKey{}, bridge)
}

// FromContext returns the foreground bridge carried by ctx, if any.
func FromContext(ctx context.Context) *Bridge {
	bridge, _ := ctx.Value(contextKey{}).(*Bridge)
	return bridge
}

// Bind installs the lifecycle hooks for the started machine.
func (b *Bridge) Bind(hooks Hooks) {
	b.mu.Lock()
	b.hooks = hooks
	b.mu.Unlock()
}

// Requests reports queued control-socket suspend work.
func (b *Bridge) Requests() <-chan struct{} {
	b.mu.RLock()
	request := b.hooks.SuspendRequests
	b.mu.RUnlock()
	if request == nil {
		return nil
	}
	return request()
}

// HandleSuspend services one queued control-socket suspend request.
func (b *Bridge) HandleSuspend(ctx context.Context) error {
	b.mu.RLock()
	handle := b.hooks.HandleSuspendRequest
	b.mu.RUnlock()
	if handle == nil {
		return nil
	}
	return handle(ctx)
}

// Commit removes restored state once the foreground session is established.
func (b *Bridge) Commit() error {
	b.mu.RLock()
	commit := b.hooks.CommitResume
	b.mu.RUnlock()
	if commit == nil {
		return nil
	}
	return commit()
}
