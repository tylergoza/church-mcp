package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeProvider struct{}

func (fakeProvider) ListTools(context.Context) ([]Tool, error) {
	return []Tool{{Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object"},
		Annotations: &Annotations{ReadOnlyHint: true}}}, nil
}

func (fakeProvider) CallTool(_ context.Context, name string, args json.RawMessage) (Result, error) {
	switch name {
	case "echo":
		return Text(string(args)), nil
	case "fail":
		return ToolError("nope"), nil
	}
	return Result{}, ErrUnknownTool
}

func post(t *testing.T, headers map[string]string, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	if err := Serve(w, r, Info{Name: "t", Version: "1", Instructions: "hi"}, fakeProvider{}); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func errCode(out map[string]any) float64 {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`

func modern(method, name string) map[string]string {
	h := map[string]string{"MCP-Protocol-Version": Modern, "Mcp-Method": method}
	if name != "" {
		h["Mcp-Name"] = name
	}
	return h
}

func TestInitializeNegotiates(t *testing.T) {
	for asked, want := range map[string]string{"2025-06-18": "2025-06-18", "2024-11-05": "2025-11-25", "2026-07-28": "2025-11-25"} {
		code, out := post(t, nil, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+asked+`"}}`)
		res, _ := out["result"].(map[string]any)
		if code != 200 || res["protocolVersion"] != want {
			t.Errorf("asked %s: %d %v", asked, code, out)
		}
	}
}

func TestLegacyCalls(t *testing.T) {
	// No header at all means 2025-03-26.
	if code, out := post(t, nil, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != 200 || out["result"] == nil {
		t.Errorf("ping: %d %v", code, out)
	}
	code, out := post(t, map[string]string{"MCP-Protocol-Version": "2025-11-25"}, `{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"fail"}}`)
	if res, _ := out["result"].(map[string]any); code != 200 || res["isError"] != true {
		t.Errorf("tool error: %d %v", code, out)
	}
	if _, out := post(t, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`); errCode(out) != codeInvalidParams {
		t.Errorf("unknown tool: %v", out)
	}
	if code, out := post(t, nil, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`); code != 200 || errCode(out) != codeMethodNotFound {
		t.Errorf("legacy unknown method: %d %v", code, out)
	}
	if code, _ := post(t, map[string]string{"MCP-Protocol-Version": "1999-01-01"}, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != 400 {
		t.Errorf("unknown legacy header: %d", code)
	}
}

func TestModernCalls(t *testing.T) {
	code, out := post(t, modern("server/discover", ""), `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`)
	res, _ := out["result"].(map[string]any)
	if code != 200 || res["resultType"] != "complete" || res["supportedVersions"] == nil {
		t.Fatalf("discover: %d %v", code, out)
	}
	code, out = post(t, modern("tools/call", "echo"), `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"x":1},`+meta+`}}`)
	if res, _ := out["result"].(map[string]any); code != 200 || !strings.Contains(fmtAny(res["content"]), `x`) {
		t.Errorf("tools/call: %d %v", code, out)
	}
	// Mcp-Name may be base64-encoded.
	enc := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("echo")) + "?="
	if code, _ := post(t, modern("tools/call", enc), `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo",`+meta+`}}`); code != 200 {
		t.Errorf("base64 Mcp-Name: %d", code)
	}
	if code, out := post(t, modern("resources/list", ""), `{"jsonrpc":"2.0","id":4,"method":"resources/list","params":{`+meta+`}}`); code != 404 || errCode(out) != codeMethodNotFound {
		t.Errorf("modern unknown method: %d %v", code, out)
	}
}

func TestModernHeaderChecks(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		body    string
		code    int
		rpc     float64
	}{
		{"missing version header", map[string]string{"Mcp-Method": "ping"}, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{` + meta + `}}`, 400, codeHeaderMismatch},
		{"method mismatch", modern("tools/list", ""), `{"jsonrpc":"2.0","id":1,"method":"ping","params":{` + meta + `}}`, 400, codeHeaderMismatch},
		{"name mismatch", modern("tools/call", "other"), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo",` + meta + `}}`, 400, codeHeaderMismatch},
		{"modern header without _meta", modern("ping", ""), `{"jsonrpc":"2.0","id":1,"method":"ping"}`, 400, codeHeaderMismatch},
		{"unsupported version", map[string]string{"MCP-Protocol-Version": "2099-01-01", "Mcp-Method": "ping"},
			`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01"}}}`, 400, codeUnsupportedVers},
	}
	for _, c := range cases {
		code, out := post(t, c.headers, c.body)
		if code != c.code || errCode(out) != c.rpc {
			t.Errorf("%s: %d %v", c.name, code, out)
		}
	}
}

func TestMessagesWithoutAnswers(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
	} {
		if code, _ := post(t, nil, body); code != http.StatusAccepted {
			t.Errorf("%s: %d", body, code)
		}
	}
	if code, out := post(t, nil, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`); code != 400 {
		t.Errorf("batch: %d %v", code, out)
	}
	if code, out := post(t, nil, `{nope`); code != 400 || errCode(out) != codeParse {
		t.Errorf("parse error: %d %v", code, out)
	}
	w := httptest.NewRecorder()
	ServeGet(w)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", w.Code)
	}
}

func fmtAny(v any) string { b, _ := json.Marshal(v); return string(b) }
