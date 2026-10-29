package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReviewClaudeStartMustPrecedeContent(t *testing.T) {
	e := newClaudeEncoder("m")
	f := claudeEmit(t, e, "text-delta", `{"text":"hello"}`)
	if f[0].Event != "message_start" {
		t.Fatalf("first frame=%s; full sequence=%v", f[0].Event, frameTypes(f))
	}
}
func TestReviewClaudeParallelToolsCannotResumeStoppedBlock(t *testing.T) {
	e := newClaudeEncoder("m")
	frames := claudeEmit(t, e, "start", `{}`)
	for _, p := range []string{`{"toolCallId":"A","name":"f","arguments":"{"}`, `{"toolCallId":"B","name":"g","arguments":"{}"}`, `{"toolCallId":"A","arguments":"}"}`} {
		frames = append(frames, claudeEmit(t, e, "tool-call-delta", p)...)
	}
	frames = append(frames, claudeEmit(t, e, "finish", `{}`)...)
	stopped := map[float64]bool{}
	for _, f := range frames {
		i, _ := f.Data["index"].(float64)
		if f.Event == "content_block_stop" {
			stopped[i] = true
		}
		if f.Event == "content_block_delta" && stopped[i] {
			t.Fatalf("delta after stop for index %v, sequence=%v", i, frameTypes(frames))
		}
	}
}
func TestReviewAuthDisabledMustSurviveParse(t *testing.T) {
	req, _ := json.Marshal(authParseRequest{FileName: "test.json", RawJSON: []byte(`{"type":"commandcode-go","api_key":"fake","disabled":true,"proxy_url":"http://proxy.invalid"}`)})
	raw, _ := handleAuthParse(req)
	var env envelope
	json.Unmarshal(raw, &env)
	var resp authParseResponse
	json.Unmarshal(env.Result, &resp)
	if !resp.Auth.Disabled {
		t.Fatalf("disabled=true input becomes Disabled=%v; storage=%s", resp.Auth.Disabled, resp.Auth.StorageJSON)
	}
}
func TestReviewToolChoiceNoneMustDisableTools(t *testing.T) {
	var req chatRequest
	json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"delete_file","parameters":{"type":"object"}}}],"tool_choice":"none"}`), &req)
	body := toCCRequest(&req, "s")
	if len(body.Params.Tools) > 0 && body.Params.ToolChoice == nil {
		t.Fatal("none becomes tools present + omitted tool_choice (auto)")
	}
}
func TestReviewVersionFailureShouldBeCached(t *testing.T) {
	setPluginConfig(nil)
	old := lookupCLIVersion
	defer func() { lookupCLIVersion = old }()
	calls := 0
	lookupCLIVersion = func(string) (string, error) { calls++; return "", fmt.Errorf("offline") }
	versionMu.Lock()
	cachedVersion = ""
	versionFetchedAt = time.Time{}
	versionMu.Unlock()
	currentCLIVersion("")
	pluginRuntime.wg.Wait()
	currentCLIVersion("")
	pluginRuntime.wg.Wait()
	if calls != 1 {
		t.Fatalf("lookups=%d", calls)
	}
}

func TestReviewExpiredQuotaShouldNotStillReportExhausted(t *testing.T) {
	pluginState = newStateStore()
	pluginState.recordRateLimit("a", "weekly", 100, 0, time.Now().Add(-time.Hour))
	q := localQuotaFallback("a", nil)
	if len(q.Groups) > 0 {
		t.Fatalf("expired weekly quota retained and exposed as %q with remaining=%v", q.Groups[0].Buckets[0].Window, q.Groups[0].Buckets[0].RemainingFraction)
	}
}
func TestReviewAssistantArrayContentMustSurvive(t *testing.T) {
	var req chatRequest
	json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"assistant","content":[{"type":"text","text":"Prior answer"}]},{"role":"user","content":"continue"}]}`), &req)
	messages, _ := toCCMessages(req.Messages)
	if len(messages) != 2 {
		t.Fatalf("assistant array content silently dropped: %+v", messages)
	}
}

func TestPumpTerminalAndSanitization(t *testing.T) {
	for _, input := range []string{"[DONE]\n", "{\"type\":\"error\",\"message\":\"bad FAKE-KEY\"}\n"} {
		t.Run(input, func(t *testing.T) {
			old := hostCall
			defer func() { hostCall = old }()
			closed := make(chan struct{})
			var once sync.Once
			reads := 0
			var delivered []byte
			hostCall = func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case "host.http.stream_read":
					reads++
					if reads == 1 {
						return marshalJSON(map[string]any{"payload": []byte(input)}), nil
					}
					<-closed
					return marshalJSON(map[string]any{"done": true}), nil
				case "host.http.stream_close":
					once.Do(func() { close(closed) })
				case "host.stream.emit", "host.stream.close":
					delivered = append(delivered, marshalJSON(payload)...)
				}
				return nil, nil
			}
			oldState := pluginState
			pluginState = newStateStore()
			defer func() { pluginState = oldState }()
			enc := newStreamEncoder("m")
			pumpStreamAsync("out", "in", time.Second, enc, enc, requestRecord{}, time.Now(), "FAKE-KEY")
			if strings.Contains(string(delivered), "FAKE-KEY") {
				t.Fatal("credential escaped")
			}
			if strings.Contains(input, "error") && pluginState.snapshot().Totals.Failures != 1 {
				t.Fatal("failure counted as success")
			}
		})
	}
}

func TestRuntimeShutdownWaitsAndRejects(t *testing.T) {
	old := hostCall
	defer func() { hostCall = old; pluginRuntime.closing = false; hostClosed = false }()
	closed := make(chan struct{})
	var once sync.Once
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		if method == "host.http.stream_close" {
			once.Do(func() { close(closed) })
		}
		return nil, nil
	}
	if !registerStream("in", "out") {
		t.Fatal("register")
	}
	go func() { <-closed; releaseStream("in") }()
	shutdownRuntime()
	if registerStream("new", "out") {
		t.Fatal("accepted during shutdown")
	}
}

func TestRuntimeReinitialize(t *testing.T) {
	old := hostCall
	hostCall = func(string, any) (json.RawMessage, error) { return nil, nil }
	defer func() { hostCall = old }()
	shutdownRuntime()
	initializeRuntime()
	if !registerStream("reinit", "out") {
		t.Fatal("reinitialized runtime rejected stream")
	}
	releaseStream("reinit")
}
func TestQuotaIdentitySwitchAndWindows(t *testing.T) {
	old := pluginState
	pluginState = newStateStore()
	defer func() { pluginState = old }()
	bindQuotaIdentity("switch", "keyA")
	storeQuotaCache("keyA", quotaFetchResponse{})
	pluginState.recordRateLimit("switch", "weekly", 3, 0, time.Now().Add(time.Hour))
	pluginState.recordRateLimit("switch", "fiveHour", 2.5, 1.25, time.Now().Add(time.Hour))
	pluginState.recordRateLimit("switch", "5h", 2.5, 1.5, time.Now().Add(time.Hour))
	q := localQuotaFallback("switch", nil)
	if len(q.Groups) != 2 {
		t.Fatalf("windows: %+v", q.Groups)
	}
	bindQuotaIdentity("switch", "keyB")
	bindQuotaIdentity("switch", "keyA")
	if _, ok := lookupQuotaCache("switch"); !ok {
		t.Fatal("management alias lost")
	}
	if len(localQuotaFallback("switch", nil).Groups) != 0 {
		t.Fatal("old calibration survived identity change")
	}
}
func TestAggregateStructuredError(t *testing.T) {
	agg := &aggregateResult{}
	applyAggregateEvent(agg, ccEvent{Type: "error", Data: map[string]any{"error": "failure"}})
	if agg.UpstreamErr == "" {
		t.Fatal("structured error counted as success")
	}
}

func TestEscapedNoneDisablesTools(t *testing.T) {
	var req chatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","tool_choice":"\u006eone","tools":[{"type":"function","function":{"name":"f"}}]}`), &req); err != nil {
		t.Fatal(err)
	}
	if len(toCCRequest(&req, "s").Params.Tools) != 0 {
		t.Fatal("escaped none ignored")
	}
}

func TestShutdownReleasesBlockedPump(t *testing.T) {
	for _, blockOn := range []string{"read", "emit"} {
		t.Run(blockOn, func(t *testing.T) {
			initializeRuntime()
			old := hostCall
			defer func() { hostCall = old; initializeRuntime() }()
			blocked := make(chan struct{})
			closed := make(chan struct{})
			var once, closeOnce sync.Once
			reads := 0
			hostCall = func(method string, _ any) (json.RawMessage, error) {
				switch method {
				case "host.http.stream_read":
					reads++
					if blockOn == "emit" && reads == 1 {
						return marshalJSON(map[string]any{"payload": []byte("{\"type\":\"text-delta\",\"text\":\"x\"}\n")}), nil
					}
					if blockOn == "read" {
						once.Do(func() { close(blocked) })
					}
					<-closed
					return nil, fmt.Errorf("closed")
				case "host.stream.emit":
					if blockOn == "emit" {
						once.Do(func() { close(blocked) })
						<-closed
						return nil, &hostError{Detail: "stream is not open"}
					}
				case "host.stream.close":
					closeOnce.Do(func() { close(closed) })
				}
				return nil, nil
			}
			if !registerStream("shutdown-in", "shutdown-out") {
				t.Fatal("register")
			}
			go func() {
				defer releaseStream("shutdown-in")
				e := newStreamEncoder("m")
				pumpStreamAsync("shutdown-out", "shutdown-in", time.Second, e, e, requestRecord{}, time.Now(), "")
			}()
			<-blocked

			shutdownRuntime()
		})
	}
}

func TestReReviewConcurrentCredentialFallback(t *testing.T) {
	quotaCache.items = map[string]quotaCacheEntry{}
	quotaIdentities = map[string]string{}
	pluginState = newStateStore()
	planCache.items = map[string]planCacheEntry{}
	old := hostCall
	defer func() { hostCall = old }()
	setPluginConfig([]byte("cli_version: 1.0.0"))
	started := make(chan struct{})
	resume := make(chan struct{})
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(hostHTTPRequestMsg)
		if strings.HasSuffix(req.URL, "/credits") && req.Headers["Authorization"][0] == "Bearer A" {
			close(started)
			<-resume
			return marshalJSON(hostHTTPResponse{StatusCode: 401}), nil
		}
		body := fmt.Sprintf(`{"windowLimits":{"limited":true,"fiveHour":{"used":20,"cap":100,"resetAt":%d}}}`, time.Now().Add(time.Hour).UnixMilli())
		return marshalJSON(hostHTTPResponse{StatusCode: 200, Body: []byte(body)}), nil
	}
	fetch := func(key string) []byte {
		storage, _ := json.Marshal(keyStorage{APIKey: key})
		req, _ := json.Marshal(quotaFetchRequest{AuthID: "a", StorageJSON: storage})
		out, _ := handleQuotaFetch(req)
		return out
	}
	result := make(chan []byte, 1)
	go func() { result <- fetch("A") }()
	<-started
	fetch("B")
	close(resume)
	raw := <-result
	var env envelope
	json.Unmarshal(raw, &env)
	var out quotaFetchResponse
	json.Unmarshal(env.Result, &out)
	if len(out.Groups) > 0 {
		t.Fatalf("old account A failure returns new account B calibration: %+v", out.Groups)
	}
}
