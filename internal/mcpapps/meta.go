package mcpapps

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// MetaKey is the `_meta` key under which fir passes raw MCP data on ACP
// tool-call updates and in agentCapabilities (fir docs/acp-mcp-passthrough.md).
const MetaKey = "dev.fir.mcp/v1"

// Extension method names of fir's dev.fir.mcp/v1 ACP extension.
const (
	MethodResourcesRead = "_fir/mcp/resources/read"
	MethodToolsCall     = "_fir/mcp/tools/call"
)

// call is what one ACP tool-call notification (start or update) tells us.
type call struct {
	id     string
	title  string
	status string // "" on a start; acp.ToolCallStatus on an update
	server string
	tool   string
	uri    string   // toolMeta.ui.resourceUri; "" if not an app tool
	vis    []string // toolMeta.ui.visibility; nil if not stated
	input  json.RawMessage
	result json.RawMessage // CallToolResult, built on a completed update
}

// firMeta is the dev.fir.mcp/v1 object.
type firMeta struct {
	Server            string          `json:"server"`
	Tool              string          `json:"tool"`
	ToolMeta          toolMeta        `json:"toolMeta"`
	ResultMeta        json.RawMessage `json:"resultMeta"`
	StructuredContent json.RawMessage `json:"structuredContent"`
}

type toolMeta struct {
	UI struct {
		ResourceURI string   `json:"resourceUri"`
		Visibility  []string `json:"visibility"`
	} `json:"ui"`
}

// parseCall reads a tool call out of u. ok is false when u is not a tool
// call or carries no dev.fir.mcp/v1 data.
func parseCall(u acp.SessionUpdate) (c call, ok bool) {
	var meta map[string]any
	switch {
	case u.ToolCall != nil:
		t := u.ToolCall
		meta, c.id, c.title = t.Meta, string(t.ToolCallId), t.Title
		c.input = rawJSON(t.RawInput)
	case u.ToolCallUpdate != nil:
		t := u.ToolCallUpdate
		meta, c.id = t.Meta, string(t.ToolCallId)
		if t.Title != nil {
			c.title = *t.Title
		}
		if t.Status != nil {
			c.status = string(*t.Status)
		}
		c.input = rawJSON(t.RawInput)
	default:
		return c, false
	}
	m, ok := meta[MetaKey]
	if !ok {
		return c, false
	}
	var fm firMeta
	if json.Unmarshal(rawJSON(m), &fm) != nil {
		return c, false
	}
	c.server, c.tool = fm.Server, fm.Tool
	c.uri, c.vis = fm.ToolMeta.UI.ResourceURI, fm.ToolMeta.UI.Visibility
	if u.ToolCallUpdate != nil && c.status == string(acp.ToolCallStatusCompleted) {
		c.result = buildResult(u.ToolCallUpdate.Content, fm)
	}
	return c, true
}

// buildResult reassembles the MCP CallToolResult the app expects from an
// ACP completed update: the content blocks (ACP and MCP share their
// shape), structuredContent and the result _meta. rawOutput is not read:
// its shape belongs to the agent and changed in fir 1.42.0.
func buildResult(content []acp.ToolCallContent, fm firMeta) json.RawMessage {
	blocks := []json.RawMessage{}
	for _, tc := range content {
		if tc.Content != nil {
			blocks = append(blocks, rawJSON(tc.Content.Content))
		}
	}
	r := map[string]any{"content": blocks}
	if len(fm.StructuredContent) > 0 {
		r["structuredContent"] = fm.StructuredContent
	}
	if len(fm.ResultMeta) > 0 {
		r["_meta"] = fm.ResultMeta
	}
	return rawJSON(r)
}

// rawJSON marshals v; nil stays nil.
func rawJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	return mustJSON(json.Marshal(v))
}

// appVisible reports whether a tool with this ui.visibility may be called
// by an app. MCP Apps: absent visibility means ["model", "app"].
func appVisible(vis []string) bool {
	if vis == nil {
		return true
	}
	for _, v := range vis {
		if v == "app" {
			return true
		}
	}
	return false
}

// resource is the app HTML and its declared policy, from resources/read.
type resource struct {
	html string
	csp  string
}

type readResult struct {
	Contents []struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType"`
		Text     string `json:"text"`
		Meta     struct {
			UI struct {
				CSP cspMeta `json:"csp"`
			} `json:"ui"`
		} `json:"_meta"`
	} `json:"contents"`
}

type cspMeta struct {
	ConnectDomains  []string `json:"connectDomains"`
	ResourceDomains []string `json:"resourceDomains"`
	FrameDomains    []string `json:"frameDomains"`
	BaseURIDomains  []string `json:"baseUriDomains"`
}

// parseResource takes the HTML text of the content entry for uri.
func parseResource(raw json.RawMessage, uri string) (resource, bool) {
	var rr readResult
	if json.Unmarshal(raw, &rr) != nil {
		return resource{}, false
	}
	for _, c := range rr.Contents {
		if c.URI == uri && c.Text != "" && strings.HasPrefix(c.MimeType, "text/html") {
			return resource{html: c.Text, csp: buildCSP(c.Meta.UI.CSP)}, true
		}
	}
	return resource{}, false
}

// domainRE is what a CSP source from a resource may look like. Anything
// else (quotes, semicolons, spaces, keywords) is dropped: the resource
// must not be able to inject its own directives.
var domainRE = regexp.MustCompile(`^(https|wss)://(\*\.)?[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*(:[0-9]{1,5})?$`)

func sources(ds []string) string {
	var ok []string
	for _, d := range ds {
		if domainRE.MatchString(d) {
			ok = append(ok, d)
		}
	}
	sort.Strings(ok)
	if len(ok) == 0 {
		return "'none'"
	}
	return strings.Join(ok, " ")
}

// buildCSP is the Content-Security-Policy for the app document: default
// deny, inline script and style allowed (an app is one HTML file), and
// only the domains the resource declares. The `sandbox` directive makes
// the document an opaque origin even when it is opened outside the
// shell's iframe.
func buildCSP(m cspMeta) string {
	res := ""
	if s := sources(m.ResourceDomains); s != "'none'" {
		res = " " + s
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src 'unsafe-inline'" + res,
		"style-src 'unsafe-inline'" + res,
		"img-src data: blob:" + res,
		"font-src data:" + res,
		"media-src data: blob:" + res,
		"connect-src " + sources(m.ConnectDomains),
		"frame-src " + sources(m.FrameDomains),
		"base-uri " + sources(m.BaseURIDomains),
		"form-action 'none'",
		"sandbox allow-scripts allow-forms",
	}, "; ")
}
