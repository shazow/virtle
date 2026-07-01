package control

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
)

// Client sends typed requests to a control socket.
type Client struct {
	dial func(context.Context) (net.Conn, error)
}

// Dial returns a client for the control socket at path.
func Dial(path string) *Client {
	return &Client{dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
}

// Status sends a status request.
func (c *Client) Status(ctx context.Context, req StatusRequest) (StatusResponse, error) {
	return callTyped[StatusRequest, StatusResponse](c, ctx, rpcStatus, req)
}

// Methods sends a methods request.
func (c *Client) Methods(ctx context.Context, req MethodsRequest) (MethodsResponse, error) {
	return callTyped[MethodsRequest, MethodsResponse](c, ctx, rpcMethods, req)
}

// Suspend sends a suspend request.
func (c *Client) Suspend(ctx context.Context, req SuspendRequest) (SuspendResponse, error) {
	return callTyped[SuspendRequest, SuspendResponse](c, ctx, rpcSuspend, req)
}

// Hotplug sends a hotplug request.
func (c *Client) Hotplug(ctx context.Context, req HotplugRequest) (HotplugResponse, error) {
	return callTyped[HotplugRequest, HotplugResponse](c, ctx, rpcHotplug, req)
}

// Balloon sends a balloon request.
func (c *Client) Balloon(ctx context.Context, req BalloonRequest) (BalloonResponse, error) {
	return callTyped[BalloonRequest, BalloonResponse](c, ctx, rpcBalloon, req)
}

// GuestPS sends a guest process list request.
func (c *Client) GuestPS(ctx context.Context, req GuestPSRequest) (GuestPSResponse, error) {
	return callTyped[GuestPSRequest, GuestPSResponse](c, ctx, rpcGuestPS, req)
}

// GuestExec sends a guest process execution request.
func (c *Client) GuestExec(ctx context.Context, req GuestExecRequest) (GuestExecResponse, error) {
	return callTyped[GuestExecRequest, GuestExecResponse](c, ctx, rpcGuestExec, req)
}

// GuestRead sends a guest file read request.
func (c *Client) GuestRead(ctx context.Context, req GuestReadRequest) (GuestReadResponse, error) {
	return callTyped[GuestReadRequest, GuestReadResponse](c, ctx, rpcGuestRead, req)
}

// GuestWrite sends a guest file write request.
func (c *Client) GuestWrite(ctx context.Context, req GuestWriteRequest) (GuestWriteResponse, error) {
	return callTyped[GuestWriteRequest, GuestWriteResponse](c, ctx, rpcGuestWrite, req)
}

// Raw sends a request to method with raw JSON params and returns the raw JSON result.
func (c *Client) Raw(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	var resp json.RawMessage
	err := c.call(ctx, rpcMethod(method), params, &resp)
	return resp, err
}

func callTyped[Req any, Resp any](c *Client, ctx context.Context, method rpcMethod, req Req) (Resp, error) {
	var resp Resp
	err := c.call(ctx, method, req, &resp)
	return resp, err
}

func (c *Client) call(ctx context.Context, method rpcMethod, params any, result any) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("control dial: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req := requestEnvelope{ID: 1, Method: method, Params: payload}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("control request: %w", err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("control response: %w", err)
	}
	var resp responseEnvelope
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("control response: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("control result: %w", err)
	}
	return nil
}
