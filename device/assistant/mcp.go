package main

import "time"

// MCP — MCP-инструменты, которые предоставляет LiteLLM MCP Gateway. Адреса и ключи самих MCP-серверов
// остаются в LiteLLM: здесь только их alias.
type MCP struct {
	Enabled bool        `json:"enabled"`
	Servers []MCPServer `json:"servers"`
}

type MCPServer struct {
	Name    string `json:"name"` // alias сервера в LiteLLM
	Enabled bool   `json:"enabled"`
}

// normalized отбрасывает неверные alias и повторы, оставляет не больше maxMCPServers.
func (m MCP) normalized() MCP {
	seen := map[string]bool{}
	out := make([]MCPServer, 0, len(m.Servers))
	for _, s := range m.Servers {
		if !mcpAlias.MatchString(s.Name) || seen[s.Name] || len(out) >= maxMCPServers {
			continue
		}
		seen[s.Name] = true
		out = append(out, s)
	}
	m.Servers = out
	return m
}

// tools — descriptors включённых серверов для поля tools. require_approval всегда never:
// подтвердить вызов голосом нечем.
func (m MCP) tools() []map[string]any {
	if !m.Enabled {
		return nil
	}
	var tools []map[string]any
	for _, s := range m.Servers {
		if s.Enabled {
			tools = append(tools, map[string]any{"type": "mcp", "server_label": s.Name,
				"server_url": "litellm_proxy/mcp/" + s.Name, "require_approval": "never"})
		}
	}
	return tools
}

// chatOpts — то, что нужно запросу к модели сверх сообщений.
type chatOpts struct {
	Tools      []map[string]any
	Timeout    time.Duration // без MCP
	MCPTimeout time.Duration // с MCP-инструментами
}

func (o chatOpts) timeout() time.Duration {
	t := o.Timeout
	if len(o.Tools) > 0 {
		t = o.MCPTimeout
	}
	if t <= 0 { // бесконечного ожидания не бывает
		t = defaultRequestTimeout * time.Second
		if len(o.Tools) > 0 {
			t = defaultMCPRequestTimeout * time.Second
		}
	}
	return t
}

func (c Config) chatOpts() chatOpts {
	return chatOpts{Tools: c.MCP.tools(),
		Timeout: time.Duration(c.RequestTimeout) * time.Second, MCPTimeout: time.Duration(c.MCPRequestTimeout) * time.Second}
}
