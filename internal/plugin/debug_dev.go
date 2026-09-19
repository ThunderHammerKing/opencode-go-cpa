//go:build debug

package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func debugBodyMeta(body []byte) string {
	if len(body) == 0 {
		return "len=0 json=false first=- last=-"
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			offset := int(syntaxErr.Offset)
			if offset > 0 && offset <= len(body) {
				return fmt.Sprintf("len=%d json=false error_offset=%d error_byte=%q first=%q last=%q", len(body), offset, body[offset-1], body[0], body[len(body)-1])
			}
			return fmt.Sprintf("len=%d json=false error_offset=%d first=%q last=%q", len(body), offset, body[0], body[len(body)-1])
		}
		return fmt.Sprintf("len=%d json=false error=%T first=%q last=%q", len(body), err, body[0], body[len(body)-1])
	}
	return fmt.Sprintf("len=%d json=true first=%q last=%q", len(body), body[0], body[len(body)-1])
}

var debugMu sync.Mutex

// debugTrace is temporary diagnosis instrumentation. It records only control
// flow and non-secret metadata; credentials, headers, prompts, and bodies are
// deliberately excluded.
func debugTrace(format string, args ...any) {
	debugMu.Lock()
	defer debugMu.Unlock()

	path := filepath.Join(filepath.Dir(os.Args[0]), "opencode-go-debug.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	values := append([]any{time.Now().Format(time.RFC3339Nano)}, args...)
	_, _ = fmt.Fprintf(f, "%s "+format+"\n", values...)
}
