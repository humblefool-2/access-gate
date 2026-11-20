// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	accessgate "github.com/humblefool-2/access-gate/lib"
)

func (e errStatus) HTTPStatus() int { return e.code }

type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(p []byte) (int, error) { return r.body.Write(p) }
func (r *recorder) WriteHeader(code int)        { r.code = code }

// tool exposes an existing HTTP handler as an agent tool, so routes and tools
// share one implementation. args is the JSON body; "id" becomes the path value.
func tool(h http.HandlerFunc) accessgate.ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		var in struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, errStatus{http.StatusBadRequest, "invalid JSON args"}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/", bytes.NewReader(args))
		if err != nil {
			return nil, err
		}
		req.SetPathValue("id", in.ID)
		rec := &recorder{header: http.Header{}, code: http.StatusOK}
		h(rec, req)

		var out any
		if err := json.Unmarshal(rec.body.Bytes(), &out); err != nil {
			return nil, fmt.Errorf("tool response: %w", err)
		}
		if rec.code >= 400 {
			msg := "tool failed"
			if m, ok := out.(map[string]any); ok {
				if s, ok := m["error"].(string); ok {
					msg = s
				}
			}
			return nil, errStatus{rec.code, msg}
		}
		return out, nil
	}
}
