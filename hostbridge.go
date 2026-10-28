package main

/*
#include "cgo_host.h"
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"
)

// hostCall invokes a host callback (plugin -> host direction) over the C ABI.
// The request is JSON-encoded, the response envelope is decoded; the host
// buffer is always released. A failed envelope (ok=false) is returned as a
// *hostError so callers can branch on the code and message.
func hostCall(method string, payload any) (json.RawMessage, error) {
	var raw []byte
	if payload != nil {
		var errMarshal error
		raw, errMarshal = json.Marshal(payload)
		if errMarshal != nil {
			return nil, fmt.Errorf("commandcode-go: encode %s request: %w", method, errMarshal)
		}
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var cRequest unsafe.Pointer
	if len(raw) > 0 {
		cRequest = C.CBytes(raw)
		defer C.free(cRequest)
	}

	var response C.cliproxy_buffer
	response.ptr = nil
	response.len = 0
	returnCode := C.call_host_api(cMethod, (*C.uint8_t)(cRequest), C.size_t(len(raw)), &response)

	var out []byte
	if response.ptr != nil {
		out = C.GoBytes(response.ptr, C.int(response.len))
		C.free_host_buffer(response.ptr, response.len)
	}
	if returnCode != 0 && len(out) == 0 {
		return nil, fmt.Errorf("commandcode-go: host call %s failed (rc=%d)", method, int(returnCode))
	}
	if len(out) == 0 {
		return nil, nil
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("commandcode-go: decode %s response: %w", method, errUnmarshal)
	}
	if !env.OK {
		message := "host call failed"
		if env.Error != nil && env.Error.Message != "" {
			message = env.Error.Message
		}
		return nil, &hostError{Method: method, Detail: message}
	}
	return env.Result, nil
}

// hostError reports a host callback failure. Disconnected upstream streams
// surface as "stream ... is not open" once the host aborts a canceled request.
type hostError struct {
	Method string
	Detail string
}

func (e *hostError) Error() string {
	return fmt.Sprintf("commandcode-go: host call %s: %s", e.Method, e.Detail)
}

// hostStreamGone reports whether the emit/close failure means the client-facing
// stream bridge is already closed (downstream disconnected or host canceled).
func hostStreamGone(err error) bool {
	if err == nil {
		return false
	}
	var he *hostError
	if !asHostError(err, &he) {
		return false
	}
	return contains(he.Detail, "is not open") || contains(he.Detail, "context canceled")
}

func asHostError(err error, target **hostError) bool {
	for err != nil {
		if he, ok := err.(*hostError); ok {
			*target = he
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// --- host.http.* wrappers ---------------------------------------------------

// hostHTTPRequestMsg mirrors rpcHostHTTPRequest on the wire.
type hostHTTPRequestMsg struct {
	Method         string              `json:"method,omitempty"`
	URL            string              `json:"url,omitempty"`
	Headers        map[string][]string `json:"headers,omitempty"`
	Body           []byte              `json:"body,omitempty"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

// hostHTTPResponse mirrors pluginapi.HTTPResponse (PascalCase, no tags).
type hostHTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

// hostHTTPDo performs a plain HTTP request through the host transport.
func hostHTTPDo(callbackID string, request hostHTTPRequestMsg) (hostHTTPResponse, error) {
	request.HostCallbackID = callbackID
	result, errCall := hostCall("host.http.do", request)
	if errCall != nil {
		return hostHTTPResponse{}, errCall
	}
	var response hostHTTPResponse
	if len(result) > 0 {
		if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
			return hostHTTPResponse{}, fmt.Errorf("commandcode-go: decode host.http.do result: %w", errUnmarshal)
		}
	}
	return response, nil
}

// hostHTTPStreamOpen mirrors rpcHostHTTPStreamResponse from host.http.do_stream.
type hostHTTPStreamOpen struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id,omitempty"`
}

// hostHTTPDoStream opens a streaming HTTP request through the host transport.
// The returned stream must be drained (hostHTTPStreamRead) and closed
// (hostHTTPStreamClose) to release the upstream connection.
func hostHTTPDoStream(callbackID string, request hostHTTPRequestMsg) (hostHTTPStreamOpen, error) {
	request.HostCallbackID = callbackID
	result, errCall := hostCall("host.http.do_stream", request)
	if errCall != nil {
		return hostHTTPStreamOpen{}, errCall
	}
	var open hostHTTPStreamOpen
	if errUnmarshal := json.Unmarshal(result, &open); errUnmarshal != nil {
		return hostHTTPStreamOpen{}, fmt.Errorf("commandcode-go: decode host.http.do_stream result: %w", errUnmarshal)
	}
	return open, nil
}

// hostHTTPStreamRead pulls the next chunk from an open host HTTP stream.
func hostHTTPStreamRead(streamID string) (payload []byte, done bool, streamErr string, err error) {
	var read struct {
		Payload []byte `json:"payload,omitempty"`
		Error   string `json:"error,omitempty"`
		Done    bool   `json:"done,omitempty"`
	}
	result, errCall := hostCall("host.http.stream_read", struct {
		StreamID string `json:"stream_id"`
	}{StreamID: streamID})
	if errCall != nil {
		return nil, true, "", errCall
	}
	if len(result) > 0 {
		if errUnmarshal := json.Unmarshal(result, &read); errUnmarshal != nil {
			return nil, true, "", fmt.Errorf("commandcode-go: decode host.http.stream_read result: %w", errUnmarshal)
		}
	}
	return read.Payload, read.Done, read.Error, nil
}

// hostHTTPStreamClose releases a host HTTP stream resource.
func hostHTTPStreamClose(streamID string) {
	if streamID == "" {
		return
	}
	_, _ = hostCall("host.http.stream_close", struct {
		StreamID string `json:"stream_id"`
	}{StreamID: streamID})
}

// --- host.stream.* wrappers --------------------------------------------------

// hostStreamEmit pushes one executor chunk to the host stream bridge.
func hostStreamEmit(streamID string, payload []byte) error {
	_, errCall := hostCall("host.stream.emit", struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload,omitempty"`
		Error    string `json:"error,omitempty"`
	}{StreamID: streamID, Payload: payload})
	return errCall
}

// hostStreamClose ends the executor stream; a non-empty error message is
// delivered to the host as a terminal stream error.
func hostStreamClose(streamID, errorMessage string) {
	if streamID == "" {
		return
	}
	_, _ = hostCall("host.stream.close", struct {
		StreamID string `json:"stream_id"`
		Error    string `json:"error,omitempty"`
	}{StreamID: streamID, Error: errorMessage})
}

// --- host.log ----------------------------------------------------------------

func hostLog(level, message string) {
	_, _ = hostCall("host.log", struct {
		Level   string `json:"level,omitempty"`
		Message string `json:"message,omitempty"`
	}{Level: level, Message: message})
}
