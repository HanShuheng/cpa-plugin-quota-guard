// Command quota-guard is a CLIProxyAPI native plugin that keeps one Codex
// OAuth credential from burning through its ChatGPT plan quota: it polls the
// live usage upstream, disables the credential when a quota window runs low,
// and re-enables it once the window recovers. Build with -buildmode=c-shared;
// CPA loads the shared library and calls the exported cliproxy_plugin_init.
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
	"errors"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const abiVersion uint32 = pluginabi.ABIVersion

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

var (
	instanceMu sync.Mutex
	instance   *Guard
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(hostAPI *C.cliproxy_host_api, pluginAPI *C.cliproxy_plugin_api) C.int {
	if pluginAPI == nil {
		return 1
	}
	C.store_host_api(hostAPI)
	pluginAPI.abi_version = C.uint32_t(abiVersion)
	pluginAPI.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	pluginAPI.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	pluginAPI.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)

	instanceMu.Lock()
	instance = NewGuard(callHost, version)
	instanceMu.Unlock()
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeCResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 0
	}
	req := C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	instanceMu.Lock()
	g := instance
	instanceMu.Unlock()
	if g == nil {
		writeCResponse(response, errorEnvelope("not_initialized", "plugin is not initialized"))
		return 0
	}
	writeCResponse(response, g.Handle(C.GoString(method), req))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, size C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = size
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	instanceMu.Lock()
	g := instance
	instanceMu.Unlock()
	if g != nil {
		g.Shutdown()
	}
}

func writeCResponse(response *C.cliproxy_buffer, payload []byte) {
	if response == nil || len(payload) == 0 {
		return
	}
	buf := C.CBytes(payload)
	response.ptr = buf
	response.len = C.size_t(len(payload))
}

// callHost sends one host callback and returns the response envelope.
func callHost(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var request *C.uint8_t
	if len(payload) > 0 {
		cPayload := C.CBytes(payload)
		if cPayload == nil {
			return nil, errors.New("allocate host payload")
		}
		defer C.free(cPayload)
		request = (*C.uint8_t)(cPayload)
	}
	var response C.cliproxy_buffer
	code := C.call_host_api(cMethod, request, C.size_t(len(payload)), &response)
	var out []byte
	if response.ptr != nil && response.len > 0 {
		out = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(out) == 0 && code != 0 {
		return nil, errors.New("host callback " + method + " failed")
	}
	return out, nil
}
