// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	accessgate "github.com/humblefool-2/access-gate/lib"
)

// A minimal MCP server over stdio (newline-delimited JSON-RPC 2.0), standard
// library only. It shows that an agent runtime needs nothing beyond the
// guard's ListTools and Call: the adapter maps tools/list and tools/call onto
// them and adds no access logic of its own. Run it with `go run . mcp`.
//
// One process is one principal and one session (the MCP client is one agent
// run). MCP_TOKEN picks the principal through the service's own authenticator
// (default dev-read; use dev-write for write tools); MCP_SESSION names the
// session (default mcp-<pid>). Logs go to stderr; stdout carries only protocol.

const mcpProtocolVersion = "2024-11-05"

// runMCP is the `mcp` subcommand.
func runMCP(db *sql.DB, authn accessgate.Authenticator) error {
	token := os.Getenv("MCP_TOKEN")
	if token == "" {
		token = "dev-read"
	}
	session := os.Getenv("MCP_SESSION")
	if session == "" {
		session = fmt.Sprintf("mcp-%d", os.Getpid())
	}
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Session-ID", session)
	p, err := authn(req)
	if err != nil {
		return fmt.Errorf("MCP_TOKEN: %w", err)
	}
	return serveMCP(newGuard(&app{db: db}), p, os.Stdin, os.Stdout)
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"` // absent on notifications
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// serveMCP answers requests from r until it ends. Every tool call is made as
// principal p, through the guard.
func serveMCP(g *accessgate.Guard, p accessgate.Principal, r io.Reader, w io.Writer) error {
	ctx := accessgate.WithPrincipal(context.Background(), p)
	enc := json.NewEncoder(w)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			if err := enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}}); err != nil {
				return err
			}
			continue
		}
		result, rerr := handleMCP(ctx, g, req)
		if len(req.ID) == 0 {
			continue // a notification gets no response
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
		if rerr != nil {
			resp.Result = nil
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func handleMCP(ctx context.Context, g *accessgate.Guard, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var in struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &in)
		version := in.ProtocolVersion
		if version == "" {
			version = mcpProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "wallet-service", "version": "0.1.0"},
		}, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		list, err := g.ListTools(ctx)
		if err != nil {
			return nil, &rpcError{-32603, err.Error()}
		}
		tools := make([]map[string]any, 0, len(list))
		for _, t := range list {
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
		}
		return map[string]any{"tools": tools}, nil

	case "tools/call":
		var in struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &in); err != nil || in.Name == "" {
			return nil, &rpcError{-32602, "tools/call needs a name"}
		}
		out, err := g.Call(ctx, in.Name, in.Arguments)
		switch {
		case errors.Is(err, accessgate.ErrUnknownTool):
			return nil, &rpcError{-32602, "unknown tool: " + in.Name}
		case err != nil:
			// A refused or failed call is a tool result the model can read, not a
			// protocol error (MCP: isError).
			return toolText(toolErrorMessage(err), true), nil
		}
		b, _ := json.Marshal(out)
		return toolText(string(b), false), nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

func toolText(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

// toolErrorMessage says what the model needs to recover (forbidden, stale,
// refused field, a business error) and hides anything internal.
func toolErrorMessage(err error) string {
	var es errStatus
	switch {
	case errors.Is(err, accessgate.ErrForbidden), errors.Is(err, accessgate.ErrFieldNotAllowed),
		errors.Is(err, accessgate.ErrStaleRead), errors.Is(err, accessgate.ErrNotInScope),
		errors.Is(err, accessgate.ErrUnauthenticated):
		return err.Error()
	case errors.As(err, &es):
		return es.msg
	}
	return "internal error"
}
