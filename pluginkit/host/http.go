package host

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/whexy/whexy-cpa-store/pluginkit/rpc"
)

// Doer is the buffered HTTP surface plugin code depends on, so tests can
// substitute a fake for the host.
type Doer interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

type httpRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	OperationID    string      `json:"operation_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

type operationRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
	OperationID    string `json:"operation_id,omitempty"`
}

type operationResponse struct {
	OperationID string `json:"operation_id"`
}

type streamStart struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type streamIDRequest struct {
	StreamID string `json:"stream_id"`
}

type streamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// Do performs a buffered upstream request through host.http.do.
func (c Client) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	var resp pluginapi.HTTPResponse
	errCall := c.withOperation(ctx, func(operationID string) error {
		return Invoke(pluginabi.MethodHostHTTPDo, c.httpRequest(req, operationID), &resp)
	})
	if errCall != nil {
		return pluginapi.HTTPResponse{}, errCall
	}
	return resp, nil
}

// DoStream starts a streaming upstream request through host.http.do_stream.
// The caller must Close the returned stream; cancelling ctx closes it too.
func (c Client) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (*Stream, error) {
	var start streamStart
	errCall := c.withOperation(ctx, func(operationID string) error {
		return Invoke(pluginabi.MethodHostHTTPDoStream, c.httpRequest(req, operationID), &start)
	})
	if errCall != nil {
		// ctx can end after the host opened the stream; nothing else would
		// close it.
		if start.StreamID != "" {
			_ = Invoke(pluginabi.MethodHostHTTPStreamClose, streamIDRequest{StreamID: start.StreamID}, nil)
		}
		return nil, errCall
	}
	if start.StreamID == "" {
		return nil, &CallError{Method: pluginabi.MethodHostHTTPDoStream, Message: "host returned no stream_id"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stream := &Stream{StatusCode: start.StatusCode, Headers: start.Headers, ID: start.StreamID, ctx: ctx}
	stop := context.AfterFunc(ctx, stream.Close)
	stream.mu.Lock()
	stream.stopWatch = stop
	stream.mu.Unlock()
	return stream, nil
}

func (c Client) httpRequest(req pluginapi.HTTPRequest, operationID string) httpRequest {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	return httpRequest{
		HostCallbackID: c.callbackID,
		OperationID:    operationID,
		Method:         method,
		URL:            req.URL,
		Headers:        req.Headers,
		Body:           req.Body,
	}
}

// withOperation runs call under a host HTTP operation that is cancelled when
// ctx is, because a blocked host.http.do cannot otherwise be interrupted from
// the plugin side.
func (c Client) withOperation(ctx context.Context, call func(operationID string) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}
	if ctx.Done() == nil {
		return call("")
	}
	var opened operationResponse
	if errOpen := Invoke(pluginabi.MethodHostHTTPOperationOpen, operationRequest{HostCallbackID: c.callbackID}, &opened); errOpen != nil {
		return errOpen
	}
	stop := context.AfterFunc(ctx, func() {
		_ = Invoke(pluginabi.MethodHostHTTPCancel, operationRequest{HostCallbackID: c.callbackID, OperationID: opened.OperationID}, nil)
	})
	errCall := call(opened.OperationID)
	stop()
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}
	return errCall
}

// Stream is an open host.http.do_stream response body.
type Stream struct {
	StatusCode int
	Headers    http.Header
	ID         string

	ctx       context.Context
	mu        sync.Mutex
	stopWatch func() bool
	closeOnce sync.Once
}

// Read returns the next body chunk. done reports the end of the body; a
// transport error mid-body is returned as err with done set.
func (s *Stream) Read() (payload []byte, done bool, err error) {
	if errCtx := s.ctx.Err(); errCtx != nil {
		return nil, true, errCtx
	}
	var resp streamReadResponse
	if errRead := Invoke(pluginabi.MethodHostHTTPStreamRead, streamIDRequest{StreamID: s.ID}, &resp); errRead != nil {
		if errCtx := s.ctx.Err(); errCtx != nil {
			return nil, true, errCtx
		}
		return nil, true, errRead
	}
	if resp.Error != "" {
		return resp.Payload, true, fmt.Errorf("upstream stream: %s", rpc.Redact(resp.Error))
	}
	return resp.Payload, resp.Done, nil
}

// ReadAll drains the body, for error responses.
func (s *Stream) ReadAll() ([]byte, error) {
	var body []byte
	for {
		chunk, done, errRead := s.Read()
		body = append(body, chunk...)
		if errRead != nil {
			return body, errRead
		}
		if done {
			return body, nil
		}
	}
}

// Close releases the upstream connection; it unblocks a pending Read.
func (s *Stream) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		stop := s.stopWatch
		s.mu.Unlock()
		if stop != nil {
			stop()
		}
		_ = Invoke(pluginabi.MethodHostHTTPStreamClose, streamIDRequest{StreamID: s.ID}, nil)
	})
}
