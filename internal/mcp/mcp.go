// Package mcp is a small, hand-written Model Context Protocol server for
// the Streamable HTTP transport: one POST endpoint, JSON responses, tools
// only. It serves both protocol eras on the same endpoint:
//
//   - modern (2026-07-28): no handshake; every request carries its protocol
//     version in params._meta and mirrors it in headers; server/discover.
//   - legacy (2025-03-26 to 2025-11-25): an initialize handshake picks the
//     version; later requests carry the MCP-Protocol-Version header. No
//     session IDs are minted (they are optional), so the server stays
//     stateless either way.
//
// Authentication is the caller's business: the HTTP handler authenticates
// the request, then hands Serve a Provider for that person.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
)

// Protocol versions served.
const Modern = "2026-07-28"

var (
	ModernVersions = []string{Modern}
	// LegacyVersions, newest first; initialize picks the client's if it's
	// here, else the newest.
	LegacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}
)

// SupportedVersions is every version, newest first.
func SupportedVersions() []string { return append(slices.Clone(ModernVersions), LegacyVersions...) }

// Tool is a tool's definition as listed.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations *Annotations   `json:"annotations,omitempty"`
}

type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

// Content is one text content block.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Result is a tool's answer. IsError results are tool execution errors the
// model can read and act on.
type Result struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError"`
}

// Text is a successful text result.
func Text(s string) Result { return Result{Content: []Content{{Type: "text", Text: s}}} }

// Errorf is a tool execution error.
func ToolError(s string) Result {
	return Result{Content: []Content{{Type: "text", Text: s}}, IsError: true}
}

// Provider is the tools one authenticated person may use.
type Provider interface {
	ListTools(ctx context.Context) ([]Tool, error)
	// CallTool runs a tool. ErrUnknownTool becomes a protocol error;
	// ErrUnauthorized makes Serve return it so the caller answers 401;
	// any other error becomes a generic tool error.
	CallTool(ctx context.Context, name string, args json.RawMessage) (Result, error)
}

var (
	ErrUnknownTool  = errors.New("unknown tool")
	ErrUnauthorized = errors.New("unauthorized")
)

// Info describes the server.
type Info struct {
	Name, Title, Version string
	Instructions         string
}

// JSON-RPC ---------------------------------------------------------------

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	// Present on a response the client sent (legacy only).
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error codes.
const (
	codeParse           = -32700
	codeInvalidRequest  = -32600
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	codeInternal        = -32603
	codeHeaderMismatch  = -32020
	codeUnsupportedVers = -32022
)

const (
	metaVersion = "io.modelcontextprotocol/protocolVersion"
	metaServer  = "io.modelcontextprotocol/serverInfo"
)

// MaxBody caps a request body.
const MaxBody = 1 << 20

// ServeGet answers GET and DELETE on the endpoint: no standalone streams
// or sessions here.
func ServeGet(w http.ResponseWriter) {
	w.Header().Set("Allow", "POST")
	http.Error(w, "this MCP endpoint takes POST only", http.StatusMethodNotAllowed)
}

// Serve handles one POST. It returns ErrUnauthorized, having written
// nothing, when the provider says the person's access has ended.
func Serve(w http.ResponseWriter, r *http.Request, info Info, p Provider) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		writeErr(w, http.StatusRequestEntityTooLarge, nil, codeInvalidRequest, "request too large", nil)
		return nil
	}
	var req request
	if trimmed := strings.TrimSpace(string(body)); strings.HasPrefix(trimmed, "[") {
		writeErr(w, http.StatusBadRequest, nil, codeInvalidRequest, "batches are not supported", nil)
		return nil
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, nil, codeParse, "parse error", nil)
		return nil
	}
	// A response or a notification from the client: accepted, no body.
	if req.Method == "" && (req.Result != nil || req.Error != nil) {
		w.WriteHeader(http.StatusAccepted)
		return nil
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeErr(w, http.StatusBadRequest, req.ID, codeInvalidRequest, "invalid request", nil)
		return nil
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted) // notifications need no answer
		return nil
	}

	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeErr(w, http.StatusBadRequest, req.ID, codeInvalidParams, "params must be an object", nil)
			return nil
		}
	}
	var bodyVersion string
	if v, ok := params.Meta[metaVersion]; ok {
		json.Unmarshal(v, &bodyVersion)
	}
	header := r.Header.Get("MCP-Protocol-Version")

	c := call{w: w, id: req.ID, info: info, p: p, ctx: r.Context()}
	switch {
	case req.Method == "initialize":
		c.legacy = true
		return c.initialize(req.Params)
	case bodyVersion != "":
		// Modern: headers must mirror the body.
		if header == "" {
			return c.mismatch("missing MCP-Protocol-Version header")
		}
		if header != bodyVersion {
			return c.mismatch("MCP-Protocol-Version header doesn't match _meta protocolVersion")
		}
		if !slices.Contains(ModernVersions, bodyVersion) {
			writeErr(w, http.StatusBadRequest, req.ID, codeUnsupportedVers, "Unsupported protocol version",
				map[string]any{"supported": SupportedVersions(), "requested": bodyVersion})
			return nil
		}
		if m := r.Header.Get("Mcp-Method"); m != req.Method {
			return c.mismatch("Mcp-Method header doesn't match the method")
		}
		if req.Method == "tools/call" {
			var p struct {
				Name string `json:"name"`
			}
			json.Unmarshal(req.Params, &p)
			if decodeHeader(r.Header.Get("Mcp-Name")) != p.Name {
				return c.mismatch("Mcp-Name header doesn't match params.name")
			}
		}
	default:
		// Legacy, after initialize. No header means 2025-03-26.
		c.legacy = true
		if header != "" && !slices.Contains(LegacyVersions, header) {
			if slices.Contains(ModernVersions, header) {
				return c.mismatch("requests at " + header + " must carry _meta." + metaVersion)
			}
			writeErr(w, http.StatusBadRequest, req.ID, codeUnsupportedVers, "Unsupported protocol version",
				map[string]any{"supported": SupportedVersions(), "requested": header})
			return nil
		}
	}

	switch req.Method {
	case "ping":
		c.ok(map[string]any{})
	case "server/discover":
		if c.legacy {
			c.notFound(req.Method)
			return nil
		}
		c.ok(map[string]any{
			"supportedVersions": SupportedVersions(),
			"capabilities":      capabilities(),
			"instructions":      info.Instructions,
			"_meta":             map[string]any{metaServer: serverInfo(info)},
		})
	case "tools/list":
		tools, err := p.ListTools(c.ctx)
		if errors.Is(err, ErrUnauthorized) {
			return err
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, req.ID, codeInternal, "couldn't list tools", nil)
			return nil
		}
		if tools == nil {
			tools = []Tool{}
		}
		c.ok(map[string]any{"tools": tools})
	case "tools/call":
		var tp struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &tp); err != nil || tp.Name == "" {
			c.rpcErr(codeInvalidParams, "tools/call needs a tool name", nil)
			return nil
		}
		if len(tp.Arguments) == 0 || string(tp.Arguments) == "null" {
			tp.Arguments = json.RawMessage("{}")
		}
		res, err := p.CallTool(c.ctx, tp.Name, tp.Arguments)
		switch {
		case errors.Is(err, ErrUnauthorized):
			return err
		case errors.Is(err, ErrUnknownTool):
			c.rpcErr(codeInvalidParams, "Unknown tool: "+tp.Name, nil)
			return nil
		case err != nil:
			res = ToolError("Something went wrong running " + tp.Name + ". Try again in a minute.")
		}
		if res.Content == nil {
			res.Content = []Content{}
		}
		c.ok(res)
	default:
		c.notFound(req.Method)
	}
	return nil
}

func capabilities() map[string]any {
	return map[string]any{"tools": map[string]any{"listChanged": false}}
}

func serverInfo(info Info) map[string]any {
	m := map[string]any{"name": info.Name, "version": info.Version}
	if info.Title != "" {
		m["title"] = info.Title
	}
	return m
}

type call struct {
	w      http.ResponseWriter
	id     json.RawMessage
	info   Info
	p      Provider
	ctx    context.Context
	legacy bool
}

func (c *call) initialize(params json.RawMessage) error {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	json.Unmarshal(params, &p)
	v := LegacyVersions[0]
	if slices.Contains(LegacyVersions, p.ProtocolVersion) {
		v = p.ProtocolVersion
	}
	c.ok(map[string]any{
		"protocolVersion": v,
		"capabilities":    capabilities(),
		"serverInfo":      serverInfo(c.info),
		"instructions":    c.info.Instructions,
	})
	return nil
}

func (c *call) ok(result any) {
	if !c.legacy {
		// Modern results say they're complete (not input_required).
		b, _ := json.Marshal(result)
		var m map[string]any
		json.Unmarshal(b, &m)
		m["resultType"] = "complete"
		result = m
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": c.id, "result": result})
}

func (c *call) rpcErr(code int, msg string, data any) {
	writeErr(c.w, http.StatusOK, c.id, code, msg, data)
}

func (c *call) notFound(method string) {
	status := http.StatusNotFound
	if c.legacy {
		status = http.StatusOK
	}
	writeErr(c.w, status, c.id, codeMethodNotFound, "Method not found: "+method, nil)
}

func (c *call) mismatch(msg string) error {
	writeErr(c.w, http.StatusBadRequest, c.id, codeHeaderMismatch, "Header mismatch: "+msg, nil)
	return nil
}

func writeErr(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string, data any) {
	resp := map[string]any{"jsonrpc": "2.0", "error": rpcError{Code: code, Message: msg, Data: data}}
	if len(id) > 0 {
		resp["id"] = id
	}
	writeJSON(w, status, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// decodeHeader undoes the =?base64?…?= sentinel encoding of Mcp-Name.
func decodeHeader(v string) string {
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") && len(v) >= len("=?base64??=") {
		b, err := base64.StdEncoding.DecodeString(v[len("=?base64?") : len(v)-2])
		if err == nil {
			return string(b)
		}
	}
	return v
}
