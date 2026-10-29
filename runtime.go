package main

import "sync"

// runtime owns background work and keeps host callbacks alive until it exits.
var pluginRuntime = struct {
	sync.Mutex
	closing bool
	streams map[string]string
	wg      sync.WaitGroup
}{streams: make(map[string]string)}
var hostLifetime sync.RWMutex
var hostClosed bool

func registerStream(upstream, output string) bool {
	pluginRuntime.Lock()
	defer pluginRuntime.Unlock()
	if pluginRuntime.closing {
		return false
	}
	pluginRuntime.streams[upstream] = output
	pluginRuntime.wg.Add(1)
	return true
}
func releaseStream(upstream string) {
	pluginRuntime.Lock()
	delete(pluginRuntime.streams, upstream)
	pluginRuntime.Unlock()
	pluginRuntime.wg.Done()
}
func shutdownRuntime() {
	pluginRuntime.Lock()
	pluginRuntime.closing = true
	streams := make(map[string]string)
	for k, v := range pluginRuntime.streams {
		streams[k] = v
	}
	pluginRuntime.Unlock()
	for upstream, output := range streams {
		hostStreamClose(output, "plugin shutting down")
		hostHTTPStreamClose(upstream)
	}
	pluginRuntime.wg.Wait()
	hostLifetime.Lock()
	hostClosed = true
	hostLifetime.Unlock()
}

func initializeRuntime() {
	pluginRuntime.Lock()
	defer pluginRuntime.Unlock()
	pluginRuntime.closing = false
	hostLifetime.Lock()
	hostClosed = false
	hostLifetime.Unlock()
}
