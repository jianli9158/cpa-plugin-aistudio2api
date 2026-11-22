//go:build darwin || linux

package cpa

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

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

typedef int (*cliproxy_plugin_init_fn)(const cliproxy_host_api*, cliproxy_plugin_api*);

static int mock_host_call(void* ctx, const char* method, const uint8_t* req, size_t req_len, cliproxy_buffer* resp) {
	const char* ok_json = "{\"ok\":true,\"result\":{}}";
	size_t n = strlen(ok_json);
	void* buf = malloc(n);
	memcpy(buf, ok_json, n);
	resp->ptr = buf;
	resp->len = n;
	return 0;
}

static void mock_host_free(void* ptr, size_t len) {
	if (ptr != NULL) {
		free(ptr);
	}
}

static int c_test_dlopen_plugin(const char* path, const char* method, const char* payload, char** out_json, char** err_msg) {
	void* handle = dlopen(path, RTLD_NOW | RTLD_LOCAL);
	if (handle == NULL) {
		*err_msg = strdup(dlerror());
		return -1;
	}
	if (dlsym(handle, "cliproxy_plugin_init") == NULL) {
		*err_msg = strdup("dlsym cliproxy_plugin_init failed");
		dlclose(handle);
		return -2;
	}
	cliproxy_plugin_init_fn init_fn = (cliproxy_plugin_init_fn)dlsym(handle, "cliproxyPluginInit");
	if (init_fn == NULL) {
		*err_msg = strdup("dlsym cliproxyPluginInit failed");
		dlclose(handle);
		return -2;
	}
	cliproxy_host_api host = {
		.abi_version = 1,
		.host_ctx = NULL,
		.call = mock_host_call,
		.free_buffer = mock_host_free,
	};
	cliproxy_plugin_api plugin = {0};
	if (init_fn(&host, &plugin) != 0) {
		*err_msg = strdup("cliproxyPluginInit returned non-zero");
		dlclose(handle);
		return -3;
	}
	cliproxy_buffer resp = {0};
	char* m = strdup(method);
	int rc = plugin.call(m, (uint8_t*)payload, strlen(payload), &resp);
	free(m);
	if (rc != 0 || resp.ptr == NULL) {
		*err_msg = strdup("plugin.call failed");
		plugin.shutdown();
		dlclose(handle);
		return -4;
	}
	char* result = (char*)malloc(resp.len + 1);
	memcpy(result, resp.ptr, resp.len);
	result[resp.len] = '\0';
	*out_json = result;
	plugin.free_buffer(resp.ptr, resp.len);
	plugin.shutdown();
	return 0;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

func CallSharedLibraryABI(dylibPath, method, payload string) (string, error) {
	cPath := C.CString(dylibPath)
	defer C.free(unsafe.Pointer(cPath))

	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	cPayload := C.CString(payload)
	defer C.free(unsafe.Pointer(cPayload))

	var outJSON *C.char
	var errMsg *C.char
	rc := C.c_test_dlopen_plugin(cPath, cMethod, cPayload, &outJSON, &errMsg)
	if errMsg != nil {
		defer C.free(unsafe.Pointer(errMsg))
	}
	if outJSON != nil {
		defer C.free(unsafe.Pointer(outJSON))
	}
	if rc != 0 {
		return "", fmt.Errorf("dlopen ABI call failed (rc=%d): %s", int(rc), C.GoString(errMsg))
	}
	return C.GoString(outJSON), nil
}
