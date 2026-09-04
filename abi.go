package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static void clear_host_api(void) {
	stored_host = NULL;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"unsafe"

	pluginimpl "github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/plugin"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

var (
	allowTestEndpointOverride = "false"
	nativeLifecycleMu         sync.Mutex
	globalMu                  sync.RWMutex
	globalPlugin              *pluginimpl.Plugin
	hostCalls                 hostCallGate
	clearStoredHostAPI        = func() { C.clear_host_api() }
)

type hostCallGate struct {
	mu          sync.Mutex
	stopping    bool
	calls       sync.WaitGroup
	waitStarted func()
}

func (g *hostCallGate) reset() {
	g.mu.Lock()
	g.stopping = false
	g.mu.Unlock()
}

func (g *hostCallGate) begin() (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return nil, false
	}
	g.calls.Add(1)
	var once sync.Once
	return func() { once.Do(g.calls.Done) }, true
}

func (g *hostCallGate) stopAccepting() {
	g.mu.Lock()
	g.stopping = true
	g.mu.Unlock()
}

func (g *hostCallGate) wait() {
	if g.waitStarted != nil {
		g.waitStarted()
	}
	g.calls.Wait()
}

type hostBridge struct{}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	nativeLifecycleMu.Lock()
	defer nativeLifecycleMu.Unlock()
	if host == nil || api == nil {
		return 1
	}
	C.store_host_api(host)
	hostCalls.reset()
	api.abi_version = C.uint32_t(protocol.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	globalMu.Lock()
	globalPlugin = pluginimpl.New(hostBridge{}, configuredNotifierEndpoint())
	globalMu.Unlock()
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	globalMu.RLock()
	current := globalPlugin
	globalMu.RUnlock()
	if current == nil {
		writeResponse(response, errorEnvelope("not_initialized", "plugin is not initialized"))
		return 1
	}
	result, err := current.Handle(C.GoString(method), requestBytes)
	if err != nil {
		code := "plugin_error"
		if strings.HasPrefix(err.Error(), "unknown method:") {
			code = "unknown_method"
		}
		writeResponse(response, errorEnvelope(code, sanitizeError(err)))
		return 1
	}
	raw, err := okEnvelope(result)
	if err != nil {
		writeResponse(response, errorEnvelope("encoding_error", "plugin response encoding failed"))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	nativeLifecycleMu.Lock()
	defer nativeLifecycleMu.Unlock()
	globalMu.Lock()
	current := globalPlugin
	globalPlugin = nil
	globalMu.Unlock()

	// Close native host-call admission before stopping the Go plugin. Monitor
	// shutdown drains every reconciliation that was already admitted, and the
	// gate below additionally covers any host call outside monitor lifecycle
	// accounting. The host API pointer is cleared only after all callbacks have
	// returned, so CPA may safely free its table and unload the shared object.
	hostCalls.stopAccepting()
	if current != nil {
		current.Shutdown()
	}
	hostCalls.wait()
	clearStoredHostAPI()
}

func (hostBridge) ListAuth(ctx context.Context) ([]protocol.HostAuthFileEntry, error) {
	result, err := callHost(ctx, protocol.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var response protocol.HostAuthListResponse
	if err := json.Unmarshal(result, &response); err != nil {
		return nil, errors.New("decode host.auth.list response")
	}
	return response.Files, nil
}

func (hostBridge) GetRuntime(ctx context.Context, authIndex string) (protocol.HostAuthFileEntry, error) {
	result, err := callHost(ctx, protocol.MethodHostAuthGetRuntime, protocol.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return protocol.HostAuthFileEntry{}, err
	}
	var response protocol.HostAuthGetRuntimeResponse
	if err := json.Unmarshal(result, &response); err != nil {
		return protocol.HostAuthFileEntry{}, errors.New("decode host.auth.get_runtime response")
	}
	return response.Auth, nil
}

// GetAuth returns the raw physical credential JSON for one auth index. The
// document contains OAuth tokens: callers decode only the fields required for
// a provider usage request and never log, persist, or render it.
func (hostBridge) GetAuth(ctx context.Context, authIndex string) ([]byte, error) {
	result, err := callHost(ctx, protocol.MethodHostAuthGet, protocol.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return nil, err
	}
	var response protocol.HostAuthGetResponse
	if err := json.Unmarshal(result, &response); err != nil {
		return nil, errors.New("decode host.auth.get response")
	}
	if len(response.JSON) == 0 {
		return nil, errors.New("host.auth.get returned an empty document")
	}
	return []byte(response.JSON), nil
}

// HTTPDo performs one upstream HTTP request through CPA's proxy-aware client.
// The native callback is synchronous and cannot be cancelled once entered;
// the monitor bounds its own wait and lets a stuck call drain at shutdown.
func (hostBridge) HTTPDo(ctx context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	result, err := callHost(ctx, protocol.MethodHostHTTPDo, request)
	if err != nil {
		return protocol.HostHTTPResponse{}, err
	}
	var response protocol.HostHTTPResponse
	if err := json.Unmarshal(result, &response); err != nil {
		return protocol.HostHTTPResponse{}, errors.New("decode host.http.do response")
	}
	return response, nil
}

func (hostBridge) Log(ctx context.Context, level, message string, fields map[string]any) {
	_, _ = callHost(ctx, protocol.MethodHostLog, protocol.HostLogRequest{
		Level:   level,
		Message: message,
		Fields:  fields,
	})
}

func callHost(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	finish, accepted := hostCalls.begin()
	if !accepted {
		return nil, errors.New("host callbacks are shutting down")
	}
	defer finish()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("encode host callback request")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		allocated := C.CBytes(rawPayload)
		if allocated == nil {
			return nil, errors.New("allocate host callback request")
		}
		defer C.free(allocated)
		requestPtr = (*C.uint8_t)(allocated)
	}
	code := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if code != 0 {
		return nil, fmt.Errorf("host callback %s returned code %d", method, int(code))
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response", method)
	}
	var envelope protocol.Envelope
	if err := json.Unmarshal(rawResponse, &envelope); err != nil {
		return nil, fmt.Errorf("host callback %s returned an invalid envelope", method)
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return nil, fmt.Errorf("host callback %s failed: %s", method, envelope.Error.Code)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	return envelope.Result, nil
}

func okEnvelope(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(protocol.Envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(protocol.Envelope{
		OK: false,
		Error: &protocol.EnvelopeError{
			Code:    code,
			Message: message,
		},
	})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func sanitizeError(err error) string {
	if err == nil {
		return "plugin error"
	}
	message := err.Error()
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}

func configuredNotifierEndpoint() string {
	if !strings.EqualFold(strings.TrimSpace(allowTestEndpointOverride), "true") {
		return ""
	}
	raw := strings.TrimSpace(os.Getenv("CPA_PUSHOVER_TEST_ENDPOINT"))
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return ""
	}
	host := parsed.Hostname()
	if host == "localhost" || host == "host.docker.internal" {
		return raw
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return raw
	}
	return ""
}
