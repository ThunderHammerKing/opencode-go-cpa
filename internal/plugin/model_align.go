package plugin

// model_align.go — the upstream reports its own bare model id (for example
// "deepseek-v4.1-flash") while clients request the public prefixed id
// ("opencode-go/deepseek-v4.1-flash"). CPA's usage and log views flag the
// difference, and some clients key on response.model, so the field is
// rewritten back to the requested public id on the way out.

import (
	"bytes"
)

// alignResponseModel rewrites the first `"model":"…"` value in a JSON response
// to the public id, leaving every other byte untouched (key order preserved,
// so true passthrough bodies stay comparable). Bodies without a model field,
// or already carrying the public id, pass through unchanged.
func alignResponseModel(body []byte, publicID string) []byte {
	if publicID == "" || len(body) == 0 {
		return body
	}
	const marker = `"model":"`
	i := bytes.Index(body, []byte(marker))
	if i < 0 {
		return body
	}
	start := i + len(marker)
	end := start
	for end < len(body) && body[end] != '"' {
		if body[end] == '\\' {
			end++ // skip the escaped byte
		}
		end++
	}
	if end >= len(body) {
		return body
	}
	if string(body[start:end]) == publicID {
		return body
	}
	out := make([]byte, 0, len(body)+len(publicID))
	out = append(out, body[:start]...)
	out = append(out, publicID...)
	out = append(out, body[end:]...)
	return out
}

// alignStreamEvents rewrites the model id inside SSE event payloads. A byte
// replacement on the exact `"model":"<upstream>"` pair keeps this cheap and
// avoids re-encoding every chunk; the adapters marshal compact JSON, so that
// pair is the shape that appears on the wire.
func alignStreamEvents(events [][]byte, upstreamID, publicID string) [][]byte {
	if upstreamID == "" || publicID == "" || upstreamID == publicID || len(events) == 0 {
		return events
	}
	from := []byte(`"model":"` + upstreamID + `"`)
	to := []byte(`"model":"` + publicID + `"`)
	for i, evt := range events {
		if bytes.Contains(evt, from) {
			events[i] = bytes.ReplaceAll(evt, from, to)
		}
	}
	return events
}
