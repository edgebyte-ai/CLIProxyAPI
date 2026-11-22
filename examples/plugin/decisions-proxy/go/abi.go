package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	int (*call)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
	void (*free_buffer)(void*, size_t);
} cliproxy_host_api;
typedef struct {
	uint32_t abi_version;
	int (*call)(char*, uint8_t*, size_t, cliproxy_buffer*);
	void (*free_buffer)(void*, size_t);
	void (*shutdown)(void);
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static void init_api(cliproxy_plugin_api* api, uint32_t version) {
	api->abi_version = version;
	api->call = cliproxyPluginCall;
	api->free_buffer = cliproxyPluginFree;
	api->shutdown = cliproxyPluginShutdown;
}
static int http_do(cliproxy_host_api* host, const uint8_t* req, size_t n, cliproxy_buffer* out) {
	return host->call(host->host_ctx, "host.http.do", req, n, out);
}
static void free_response(cliproxy_host_api* host, cliproxy_buffer out) {
	host->free_buffer(out.ptr, out.len);
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"math"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var hostAPI atomic.Pointer[C.cliproxy_host_api]
var service decisionsProxy

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || uint32(host.abi_version) != pluginabi.ABIVersion || host.call == nil || host.free_buffer == nil {
		return 1
	}
	hostAPI.Store(host)
	C.init_api(api, C.uint32_t(pluginabi.ABIVersion))
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, n C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil || method == nil || n > math.MaxInt32 || (n > 0 && request == nil) {
		return 1
	}
	*response = C.cliproxy_buffer{}
	result, errCall := service.dispatch(C.GoString(method), unsafe.Slice((*byte)(unsafe.Pointer(request)), int(n)), hostHTTPDo)
	envelope := pluginabi.Envelope{OK: errCall == nil}
	if errCall != nil {
		envelope.Error = errCall
	} else {
		raw, errMarshal := json.Marshal(result)
		if errMarshal != nil {
			return 1
		}
		envelope.Result = raw
	}
	raw, errMarshal := json.Marshal(envelope)
	if errMarshal != nil {
		return 1
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) { C.free(ptr) }

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	service.config.Store(nil)
	hostAPI.Store(nil)
}

func hostHTTPDo(req hostRequest) (pluginapi.HTTPResponse, error) {
	var upstream pluginapi.HTTPResponse
	host := hostAPI.Load()
	raw, errMarshal := json.Marshal(req)
	if errMarshal != nil || host == nil {
		return upstream, errors.New("host HTTP transport unavailable")
	}
	var result C.cliproxy_buffer
	rc := C.http_do(host, (*C.uint8_t)(unsafe.Pointer(&raw[0])), C.size_t(len(raw)), &result)
	if result.ptr != nil {
		defer C.free_response(host, result)
	}
	// The host returns the bounded body as base64 along with headers in a JSON envelope.
	if rc != 0 || result.ptr == nil || result.len > 3*maxResponseBytes {
		return upstream, errors.New("host HTTP transport failed")
	}
	var envelope struct {
		OK     bool
		Result pluginapi.HTTPResponse
	}
	if errDecode := json.Unmarshal(unsafe.Slice((*byte)(result.ptr), int(result.len)), &envelope); errDecode != nil || !envelope.OK {
		return upstream, errors.New("host HTTP transport failed")
	}
	return envelope.Result, nil
}
