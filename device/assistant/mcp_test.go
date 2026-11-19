package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMCPTools(t *testing.T) {
	m := MCP{Enabled: true, Servers: []MCPServer{{"deepwiki", true}, {"off", false}, {"github", true}}}
	want := []map[string]any{
		{"type": "mcp", "server_label": "deepwiki", "server_url": "litellm_proxy/mcp/deepwiki", "require_approval": "never"},
		{"type": "mcp", "server_label": "github", "server_url": "litellm_proxy/mcp/github", "require_approval": "never"},
	}
	if got := m.tools(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	m.Enabled = false
	if m.tools() != nil {
		t.Fatal("выключенный MCP не должен давать tools")
	}
}

func TestMCPConfigNormalization(t *testing.T) {
	var servers []string
	for i := 0; i < 30; i++ {
		servers = append(servers, `{"name":"s`+strings.Repeat("x", i)+`","enabled":true}`)
	}
	raw := `{"mcp":{"enabled":true,"servers":[{"name":"a","enabled":true,"require_approval":"always"},{"name":"a","enabled":false},
		{"name":"bad name"},{"name":""},{"name":"` + strings.Repeat("z", 65) + `"},` + strings.Join(servers, ",") + `]}}`
	c, err := parseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MCP.Servers) != maxMCPServers || c.MCP.Servers[0] != (MCPServer{"a", true}) {
		t.Fatalf("%v", c.MCP.Servers)
	}
	seen := map[string]bool{}
	for _, s := range c.MCP.Servers {
		if seen[s.Name] || !mcpAlias.MatchString(s.Name) {
			t.Fatalf("плохой сервер %q", s.Name)
		}
		seen[s.Name] = true
	}
	if tools := c.MCP.tools(); tools[0]["require_approval"] != "never" {
		t.Fatal("require_approval должен быть never")
	}
}

func TestTimeoutDefaultsAndBounds(t *testing.T) {
	c, _ := parseConfig([]byte(`{}`))
	if c.RequestTimeout != 90 || c.MCPRequestTimeout != 180 || c.MCP.Enabled {
		t.Fatalf("defaults: %+v", c)
	}
	for _, tc := range []struct {
		in       string
		req, mcp int
	}{
		{`{"request_timeout":10,"mcp_request_timeout":10}`, 10, 10},
		{`{"request_timeout":300,"mcp_request_timeout":600}`, 300, 600},
		{`{"request_timeout":9,"mcp_request_timeout":9}`, 90, 180},
		{`{"request_timeout":301,"mcp_request_timeout":601}`, 90, 180},
		{`{"request_timeout":-5,"mcp_request_timeout":0}`, 90, 180},
	} {
		c, err := parseConfig([]byte(tc.in))
		if err != nil || c.RequestTimeout != tc.req || c.MCPRequestTimeout != tc.mcp {
			t.Errorf("%s: %+v %v", tc.in, c, err)
		}
	}
}

func TestChatOptsTimeoutChoice(t *testing.T) {
	c, _ := parseConfig([]byte(`{"request_timeout":20,"mcp_request_timeout":200,"mcp":{"enabled":true,"servers":[{"name":"a","enabled":true}]}}`))
	if got := c.chatOpts().timeout(); got != 200*time.Second {
		t.Fatalf("с MCP: %v", got)
	}
	c.MCP.Enabled = false
	if got := c.chatOpts().timeout(); got != 20*time.Second {
		t.Fatalf("без MCP: %v", got)
	}
	if got := (chatOpts{}).timeout(); got != 90*time.Second {
		t.Fatalf("нулевой: %v", got)
	}
	if got := (chatOpts{Tools: []map[string]any{{}}}).timeout(); got != 180*time.Second {
		t.Fatalf("нулевой с MCP: %v", got)
	}
}

func chatServer(got *map[string]any, status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			json.NewDecoder(r.Body).Decode(got)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func TestChatSendsMCPToolsAndKeepsParams(t *testing.T) {
	var got map[string]any
	srv := chatServer(&got, 200, `{"choices":[{"message":{"content":"Итог после MCP"},"finish_reason":"stop"}]}`)
	defer srv.Close()
	m := MCP{Enabled: true, Servers: []MCPServer{{"deepwiki", true}, {"github", true}}}
	llm := LLM{BaseURL: srv.URL, Model: "m", MaxTokens: 123}
	ans, err := chat(context.Background(), llm, []message{{"system", "s"}, {"user", "q"}}, chatOpts{Tools: m.tools()})
	if err != nil || ans != "Итог после MCP" {
		t.Fatalf("%q %v", ans, err)
	}
	if got["model"] != "m" || got["max_tokens"] != float64(123) || got["stream"] != false || len(got["messages"].([]any)) != 2 {
		t.Fatalf("параметры потеряны: %v", got)
	}
	tools := got["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["server_url"] != "litellm_proxy/mcp/deepwiki" ||
		tools[1].(map[string]any)["server_url"] != "litellm_proxy/mcp/github" {
		t.Fatalf("tools: %v", tools)
	}
}

func TestChatWithoutMCPHasNoToolsField(t *testing.T) {
	var got map[string]any
	srv := chatServer(&got, 200, `{"choices":[{"message":{"content":"ok"}}]}`)
	defer srv.Close()
	if _, err := chat(context.Background(), LLM{BaseURL: srv.URL, Model: "m"}, []message{{"user", "q"}}, chatOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, has := got["tools"]; has || len(got) != 4 {
		t.Fatalf("запрос изменился: %v", got)
	}
}

func TestChatAPIError(t *testing.T) {
	srv := chatServer(nil, 401, `{"error":{"message":"key not allowed to access mcp server"}}`)
	defer srv.Close()
	tools := MCP{true, []MCPServer{{"a", true}}}.tools()
	_, err := chat(context.Background(), LLM{BaseURL: srv.URL, Model: "m"}, nil, chatOpts{Tools: tools})
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("%v", err)
	}
}

func TestChatTimeoutCancelsRequestAndBody(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	for name, slowBody := range map[string]bool{"headers": false, "body": true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body) // без этого сервер не заметит обрыв соединения
			if slowBody {
				w.WriteHeader(200)
				w.Write([]byte(`{"choices":`))
				w.(http.Flusher).Flush()
			}
			<-r.Context().Done()
			cancelled <- struct{}{}
		}))
		start := time.Now()
		_, err := chat(context.Background(), LLM{BaseURL: srv.URL, Model: "m"}, nil, chatOpts{Timeout: 150 * time.Millisecond})
		if err == nil || time.Since(start) > 3*time.Second {
			t.Fatalf("%s: %v за %v", name, err, time.Since(start))
		}
		select {
		case <-cancelled:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: запрос на сервере не отменён", name)
		}
		srv.Close()
	}
}

func TestChatTimeoutUsesMCPValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{"choices":[{"message":{"content":"поздно, но успел"}}]}`))
	}))
	defer srv.Close()
	tools := MCP{true, []MCPServer{{"a", true}}}.tools()
	o := chatOpts{Tools: tools, Timeout: 50 * time.Millisecond, MCPTimeout: 3 * time.Second}
	if ans, err := chat(context.Background(), LLM{BaseURL: srv.URL, Model: "m"}, nil, o); err != nil || ans == "" {
		t.Fatalf("с MCP должен ждать MCPTimeout: %v", err)
	}
	o.Tools = nil
	if _, err := chat(context.Background(), LLM{BaseURL: srv.URL, Model: "m"}, nil, o); err == nil {
		t.Fatal("без MCP должен упасть по Timeout")
	}
}
