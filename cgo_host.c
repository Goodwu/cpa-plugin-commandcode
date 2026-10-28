#include "cgo_host.h"

/* Single shared host-API shim: main.go stores the host table once, every
 * translation unit (including hostbridge.go) calls through these symbols.
 * static helpers would have per-TU copies and lose the stored pointer. */

const cliproxy_host_api* stored_host = 0;

void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == 0 || stored_host->call == 0) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != 0 && stored_host->free_buffer != 0 && ptr != 0) {
		stored_host->free_buffer(ptr, len);
	}
}
