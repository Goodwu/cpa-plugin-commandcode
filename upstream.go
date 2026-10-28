package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// cliNodeVersion is the Node.js version advertised in the CLI-identifying
// headers. The reference proxy sends its own Node runtime; the CC server
// inspects the CLI fingerprint headers and rejects requests that look stale
// or proxy-like, so we keep the official shape with a current LTS version.
const cliNodeVersion = "v22.14.0"

const maxErrorBodyBytes = 16 * 1024

// upstreamFailure is a pre-stream upstream error (retry budget exhausted or
// permanent status). Message is already key-redacted and control-char free.
type upstreamFailure struct {
	Status    int // 0 = transport error
	Message   string
	Retryable bool
}

func (e *upstreamFailure) Error() string { return e.Message }

// buildUpstreamHeaders reproduces the official CLI header set.
func buildUpstreamHeaders(apiKey, cliVersion string, body *ccRequestBody) map[string][]string {
	slug := activeConfig().ProjectSlug
	if slug == "" {
		slug = slugifyWorkingDir(body.Config["workingDir"])
	}
	return map[string][]string{
		"Content-Type":           {"application/json"},
		"Accept":                 {"application/json, */*"},
		"Accept-Encoding":        {"gzip, deflate, br"},
		"Accept-Language":        {"en-US,en;q=0.9"},
		"Connection":             {"keep-alive"},
		"User-Agent":             {fmt.Sprintf("commandcode-cli/%s Node.js/%s", cliVersion, cliNodeVersion)},
		"Authorization":          {"Bearer " + apiKey},
		"x-cli-environment":      {"production"},
		"x-command-code-version": {cliVersion},
		"x-session-id":           {body.ThreadID},
		"x-co-flag":              {"false"},
		"x-taste-learning":       {"false"},
		"x-project-slug":         {slug},
		"traceparent":            {"00-" + randomHex(16) + "-" + randomHex(8) + "-01"},
	}
}

func slugifyWorkingDir(workingDir any) string {
	base := ""
	if dir, ok := workingDir.(string); ok && dir != "" {
		trimmed := strings.TrimRight(dir, "/\\")
		if idx := strings.LastIndexAny(trimmed, "/\\"); idx >= 0 {
			base = trimmed[idx+1:]
		} else {
			base = trimmed
		}
	}
	if base == "" {
		base = "commandcode-proxy"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" {
		return "commandcode-proxy"
	}
	return slug
}

// openUpstreamStream POSTs the CC generate body and returns the opened host
// HTTP stream. Retryable failures (5xx/429/transport) are retried with linear
// backoff, but only before any chunk has been consumed.
func openUpstreamStream(callbackID, apiBase, apiKey string, body *ccRequestBody) (hostHTTPStreamOpen, error) {
	cfg := activeConfig()
	cliVersion := currentCLIVersion(callbackID)
	raw, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return hostHTTPStreamOpen{}, fmt.Errorf("commandcode-go: encode request: %w", errMarshal)
	}
	request := hostHTTPRequestMsg{
		Method:  "POST",
		URL:     strings.TrimRight(apiBase, "/") + "/alpha/generate",
		Headers: buildUpstreamHeaders(apiKey, cliVersion, body),
		Body:    raw,
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		open, errCall := hostHTTPDoStream(callbackID, request)
		if errCall != nil {
			lastErr = &upstreamFailure{
				Message:   fmt.Sprintf("CC upstream request failed: %v", errCall),
				Retryable: true,
			}
			if attempt < cfg.MaxRetries {
				hostLog("warn", fmt.Sprintf("commandcode-go: upstream transport error, retrying %d/%d", attempt+1, cfg.MaxRetries))
				time.Sleep(cfg.RetryBackoff * time.Duration(attempt+1))
				continue
			}
			return hostHTTPStreamOpen{}, lastErr
		}
		if open.StatusCode >= 200 && open.StatusCode < 300 {
			if open.StreamID == "" {
				return hostHTTPStreamOpen{}, &upstreamFailure{Message: "CC API returned no body", Retryable: true}
			}
			return open, nil
		}
		// Consume the error body (bounded) before deciding on a retry so the
		// upstream connection is released and diagnostics survive.
		errorText := drainErrorBody(open.StreamID, apiKey)
		retryable := open.StatusCode >= 500 || open.StatusCode == 429
		lastErr = &upstreamFailure{
			Status:    open.StatusCode,
			Message:   fmt.Sprintf("CC API %d: %s", open.StatusCode, sanitizeErrorText(errorText, apiKey)),
			Retryable: retryable,
		}
		if retryable && attempt < cfg.MaxRetries {
			hostLog("warn", fmt.Sprintf("commandcode-go: upstream %d, retrying %d/%d", open.StatusCode, attempt+1, cfg.MaxRetries))
			time.Sleep(cfg.RetryBackoff * time.Duration(attempt+1))
			continue
		}
		return hostHTTPStreamOpen{}, lastErr
	}
}

// drainErrorBody reads up to maxErrorBodyBytes of a failed response body. A
// prefix that could end halfway through a secret is discarded wholesale.
func drainErrorBody(streamID, apiKey string) string {
	if streamID == "" {
		return ""
	}
	defer hostHTTPStreamClose(streamID)
	var collected strings.Builder
	size := 0
	for {
		payload, done, streamErr, errRead := hostHTTPStreamRead(streamID)
		if errRead != nil || streamErr != "" {
			return "[error body unavailable]"
		}
		size += len(payload)
		if size >= maxErrorBodyBytes {
			return "[error body truncated]"
		}
		collected.Write(payload)
		if done {
			return collected.String()
		}
	}
}

// pumpUpstream reads the opened NDJSON stream, splits it into lines, parses
// CC events and invokes handle until [DONE], stream end, idle timeout, or a
// handle error (typically a downstream disconnect) stops it.
//
// The caller MUST close the stream via hostHTTPStreamClose on every exit path;
// that also cancels the host-side read and wakes the puller goroutine.
func pumpUpstream(streamID string, idleTimeout time.Duration, handle func(ccEvent) error) error {
	events := make(chan ccEvent)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		buffer := ""
		for {
			payload, done, streamErr, errRead := hostHTTPStreamRead(streamID)
			if errRead != nil {
				select {
				case errs <- errRead:
				default:
				}
				return // canceled or bridge closed
			}
			buffer += string(payload)
			for {
				idx := strings.IndexByte(buffer, '\n')
				if idx < 0 {
					break
				}
				line := buffer[:idx]
				buffer = buffer[idx+1:]
				if event, ok := parseCCLine(line); ok {
					emit := event
					select {
					case events <- emit:
					case <-errs:
						return
					}
				}
			}
			if done {
				if strings.TrimSpace(buffer) != "" {
					if event, ok := parseCCLine(buffer); ok {
						select {
						case events <- event:
						case <-errs:
						}
					}
				}
				if streamErr != "" {
					select {
					case errs <- fmt.Errorf("CC upstream stream error: %s", streamErr):
					default:
					}
				}
				return
			}
		}
	}()

	pumpErr := func() error {
		var lastActivity = time.Now()
		for {
			var idle <-chan time.Time
			if idleTimeout > 0 {
				remaining := idleTimeout - time.Since(lastActivity)
				if remaining <= 0 {
					return fmt.Errorf("CC upstream idle timeout: no data for %s", idleTimeout)
				}
				idle = time.After(remaining)
			}
			select {
			case event, ok := <-events:
				if !ok {
					select {
					case err := <-errs:
						return err
					default:
						return nil
					}
				}
				lastActivity = time.Now()
				if err := handle(event); err != nil {
					return err
				}
			case err := <-errs:
				return err
			case <-idle:
				return fmt.Errorf("CC upstream idle timeout: no data for %s", idleTimeout)
			}
		}
	}()

	if pumpErr != nil {
		// Wake the puller if it is parked on an events send: the buffered
		// error makes its `case <-errs` branches fire and it exits.
		select {
		case errs <- pumpErr:
		default:
		}
	}
	return pumpErr
}

func sanitizeErrorText(text, apiKey string) string {
	if apiKey != "" {
		text = strings.ReplaceAll(text, apiKey, "[redacted]")
	}
	var b strings.Builder
	for _, r := range text {
		if r < 0x20 && r != '\t' && r != '\n' {
			continue
		}
		if r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	result := b.String()
	if len(result) > maxErrorBodyBytes {
		result = result[:maxErrorBodyBytes]
	}
	return result
}

// newStatusError adapts an HTTP status for callers that branch on errors.
func newStatusError(status int, message string) error {
	return &upstreamFailure{Status: status, Message: message, Retryable: status >= 500 || status == 429}
}

func newUUID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(raw[0:4]),
		hex.EncodeToString(raw[4:6]),
		hex.EncodeToString(raw[6:8]),
		hex.EncodeToString(raw[8:10]),
		hex.EncodeToString(raw[10:16]))
}

func randomHex(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

func truncate(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max] + "..."
}
