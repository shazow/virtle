package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Router dispatches typed control socket requests to runtime capabilities.
type Router struct {
	methods map[rpcMethod]methodSpec
}

// NewRouter creates a router from explicit runtime capability handlers.
func NewRouter(handlers Handlers) (*Router, error) {
	if handlers.Core == nil {
		return nil, fmt.Errorf("core handler is required")
	}
	router := &Router{methods: make(map[rpcMethod]methodSpec, len(defaultMethods))}
	for _, name := range defaultMethodOrder {
		method := defaultMethods[name]
		spec, ok := method.bind(router, handlers)
		if ok {
			router.methods[name] = spec
		}
	}
	return router, nil
}

type methodSpec struct {
	handle func(context.Context, json.RawMessage) (any, error)
}

type methodRegistration struct {
	bind func(*Router, Handlers) (methodSpec, bool)
}

var (
	defaultMethodOrder []rpcMethod
	defaultMethods     = map[rpcMethod]methodRegistration{}
)

func init() {
	registerDefaultMethod(rpcStatus, typedRegistration(func(handlers Handlers) func(context.Context, StatusRequest) (StatusResponse, error) {
		if handlers.Core == nil {
			return nil
		}
		return handlers.Core.Status
	}))
	registerDefaultMethod(rpcMethods, methodRegistration{
		bind: func(router *Router, handlers Handlers) (methodSpec, bool) {
			return typedMethod(func(context.Context, MethodsRequest) (MethodsResponse, error) {
				return router.methodsResponse(), nil
			}), true
		},
	})
	registerDefaultMethod(rpcGuestPS, typedRegistration(func(handlers Handlers) func(context.Context, GuestPSRequest) (GuestPSResponse, error) {
		if handlers.Guest == nil {
			return nil
		}
		return handlers.Guest.GuestPS
	}))
	registerDefaultMethod(rpcGuestExec, typedRegistration(func(handlers Handlers) func(context.Context, GuestExecRequest) (GuestExecResponse, error) {
		if handlers.Guest == nil {
			return nil
		}
		return handlers.Guest.GuestExec
	}))
	registerDefaultMethod(rpcGuestRead, typedRegistration(func(handlers Handlers) func(context.Context, GuestReadRequest) (GuestReadResponse, error) {
		if handlers.Guest == nil {
			return nil
		}
		return handlers.Guest.GuestRead
	}))
	registerDefaultMethod(rpcGuestWrite, typedRegistration(func(handlers Handlers) func(context.Context, GuestWriteRequest) (GuestWriteResponse, error) {
		if handlers.Guest == nil {
			return nil
		}
		return handlers.Guest.GuestWrite
	}))
	registerDefaultMethod(rpcSuspend, typedRegistration(func(handlers Handlers) func(context.Context, SuspendRequest) (SuspendResponse, error) {
		if handlers.Suspend == nil {
			return nil
		}
		return handlers.Suspend.Suspend
	}))
	registerDefaultMethod(rpcHotplug, typedRegistration(func(handlers Handlers) func(context.Context, HotplugRequest) (HotplugResponse, error) {
		if handlers.Hotplug == nil {
			return nil
		}
		return handlers.Hotplug.Hotplug
	}))
	registerDefaultMethod(rpcBalloon, typedRegistration(func(handlers Handlers) func(context.Context, BalloonRequest) (BalloonResponse, error) {
		if handlers.Balloon == nil {
			return nil
		}
		return handlers.Balloon.Balloon
	}))
}

func registerDefaultMethod(name rpcMethod, method methodRegistration) {
	if name == "" {
		panic("control method name is required")
	}
	if method.bind == nil {
		panic(fmt.Sprintf("control method %q bind function is required", name))
	}
	if _, exists := defaultMethods[name]; exists {
		panic(fmt.Sprintf("control method %q registered twice", name))
	}
	defaultMethods[name] = method
	defaultMethodOrder = append(defaultMethodOrder, name)
}

func typedRegistration[Req any, Resp any](
	selector func(Handlers) func(context.Context, Req) (Resp, error),
) methodRegistration {
	return methodRegistration{
		bind: func(_ *Router, handlers Handlers) (methodSpec, bool) {
			call := selector(handlers)
			if call == nil {
				return methodSpec{}, false
			}
			return typedMethod(call), true
		},
	}
}

func typedMethod[Req any, Resp any](
	call func(context.Context, Req) (Resp, error),
) methodSpec {
	return methodSpec{
		handle: func(ctx context.Context, params json.RawMessage) (any, error) {
			var req Req
			if err := decodeParams(params, &req); err != nil {
				var zero Resp
				return zero, err
			}
			return call(ctx, req)
		},
	}
}

func (r *Router) handle(ctx context.Context, req requestEnvelope) responseEnvelope {
	spec, ok := r.methods[req.Method]
	if !ok {
		if _, known := defaultMethods[req.Method]; known {
			return responseEnvelope{
				ID:    req.ID,
				Error: &RPCError{Code: ErrUnsupported, Message: fmt.Sprintf("%s is not supported by this control socket", req.Method)},
			}
		}
		return responseEnvelope{
			ID:    req.ID,
			Error: &RPCError{Code: ErrUnknownMethod, Message: fmt.Sprintf("unknown method %q", req.Method)},
		}
	}

	resp := responseEnvelope{ID: req.ID}
	result, err := spec.handle(ctx, req.Params)
	if err != nil {
		resp.Error = rpcError(err)
		return resp
	}
	payload, err := json.Marshal(result)
	if err != nil {
		resp.Error = &RPCError{Code: ErrInternal, Message: err.Error()}
		return resp
	}
	resp.Result = payload
	return resp
}

func (r *Router) methodsResponse() MethodsResponse {
	methods := make([]string, 0, len(r.methods))
	for _, method := range defaultMethodOrder {
		if _, ok := r.methods[method]; ok {
			methods = append(methods, string(method))
		}
	}
	return MethodsResponse{Methods: methods}
}

func decodeParams(data json.RawMessage, dst any) error {
	if len(data) == 0 {
		data = []byte("{}")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return &RPCError{Code: ErrInvalidParams, Message: err.Error()}
	}
	return nil
}

func rpcError(err error) *RPCError {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	return &RPCError{Code: ErrInternal, Message: err.Error()}
}
