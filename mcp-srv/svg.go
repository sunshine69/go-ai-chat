package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type SVGToolManager struct {
	mu       sync.Mutex
	width    float64
	height   float64
	bgColor  string
	elements []string
	defs     []string
	nextID   int
}

func NewSVGToolManager() *SVGToolManager {
	return &SVGToolManager{width: 400, height: 300}
}

func (m *SVGToolManager) svgGenID(prefix string) string {
	m.nextID++
	return fmt.Sprintf("%s%d", prefix, m.nextID)
}

func svgGetArgs(req mcp.CallToolRequest) map[string]interface{} {
	if m, ok := req.Params.Arguments.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func svgStr(args map[string]interface{}, key, fallback string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return fallback
}

func svgFloat(args map[string]interface{}, key string, fallback float64) float64 {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		}
	}
	return fallback
}

func svgAttr(key, val string) string {
	return fmt.Sprintf(` %s="%s"`, key, val)
}

func svgOpt(key, val string) string {
	if val == "" {
		return ""
	}
	return fmt.Sprintf(` %s="%s"`, key, val)
}

func svgOptN(key string, val, def float64) string {
	if val == def {
		return ""
	}
	return fmt.Sprintf(` %s="%.1f"`, key, val)
}

func svgWrap(xml, transform string) string {
	if transform == "" {
		return "  " + xml
	}
	return fmt.Sprintf("  <g transform=\"%s\">\n    %s\n  </g>", transform, xml)
}

type gStop struct {
	offset, color string
}

func svgParseStops(s string) []gStop {
	var stops []gStop
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if i := strings.Index(p, ":"); i > 0 {
			off := strings.TrimSpace(p[:i])
			col := strings.TrimSpace(p[i+1:])
			if off != "" && col != "" {
				stops = append(stops, gStop{off, col})
			}
		}
	}
	return stops
}

func svgStopsXML(stops []gStop) string {
	var sb strings.Builder
	for _, s := range stops {
		sb.WriteString(fmt.Sprintf(`    <stop offset="%s" stop-color="%s"/>`+"\n", s.offset, s.color))
	}
	return sb.String()
}

// ================================================================
// HANDLERS
// ================================================================

func (m *SVGToolManager) handleCreateCanvas(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	args := svgGetArgs(req)
	m.width = svgFloat(args, "width", 400)
	m.height = svgFloat(args, "height", 300)
	m.bgColor = svgStr(args, "bg_color", "")
	m.elements = nil
	m.defs = nil
	m.nextID = 0
	r := fmt.Sprintf("Canvas created: %.0fx%.0f", m.width, m.height)
	if m.bgColor != "" {
		r += " bg:" + m.bgColor
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: r}}}, nil
}

func (m *SVGToolManager) handleAddCircle(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := svgGetArgs(req)
	id := m.svgGenID("c")
	cx, cy, r := svgFloat(a, "cx", 0), svgFloat(a, "cy", 0), svgFloat(a, "r", 10)
	s := svgAttr("id", id) + fmt.Sprintf(` cx="%.1f" cy="%.1f" r="%.1f"`, cx, cy, r) +
		svgOpt("fill", svgStr(a, "fill", "#ffffff")) + svgOpt("stroke", svgStr(a, "stroke", "")) +
		svgOptN("stroke-width", svgFloat(a, "stroke_width", 0), 0) + svgOptN("opacity", svgFloat(a, "opacity", 1), 1)
	m.elements = append(m.elements, svgWrap(fmt.Sprintf("<circle%s/>", s), svgStr(a, "transform", "")))
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: fmt.Sprintf("Circle %s at (%.0f,%.0f) r=%.0f", id, cx, cy, r)}}}, nil
}

func (m *SVGToolManager) handleAddRect(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := svgGetArgs(req)
	id := m.svgGenID("r")
	x, y := svgFloat(a, "x", 0), svgFloat(a, "y", 0)
	w, h := svgFloat(a, "width", 100), svgFloat(a, "height", 100)
	s := svgAttr("id", id) + fmt.Sprintf(` x="%.1f" y="%.1f" width="%.1f" height="%.1f"`, x, y, w, h) +
		svgOpt("fill", svgStr(a, "fill", "#ffffff")) + svgOpt("stroke", svgStr(a, "stroke", "")) +
		svgOptN("stroke-width", svgFloat(a, "stroke_width", 0), 0) +
		svgOptN("rx", svgFloat(a, "rx", 0), 0) + svgOptN("ry", svgFloat(a, "ry", 0), 0) +
		svgOptN("opacity", svgFloat(a, "opacity", 1), 1)
	m.elements = append(m.elements, svgWrap(fmt.Sprintf("<rect%s/>", s), svgStr(a, "transform", "")))
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: fmt.Sprintf("Rect %s (%.0f,%.0f) %.0fx%.0f", id, x, y, w, h)}}}, nil
}

func (m *SVGToolManager) handleAddEllipse(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := svgGetArgs(req)
	id := m.svgGenID("e")
	cx, cy := svgFloat(a, "cx", 0), svgFloat(a, "cy", 0)
	rx, ry := svgFloat(a, "rx", 50), svgFloat(a, "ry", 30)
	s := svgAttr("id", id) + fmt.Sprintf(` cx="%.1f" cy="%.1f" rx="%.1f" ry="%.1f"`, cx, cy, rx, ry) +
		svgOpt("fill", svgStr(a, "fill", "#ffffff")) + svgOpt("stroke", svgStr(a, "stroke", "")) +
		svgOptN("stroke-width", svgFloat(a, "stroke_width", 0), 0) + svgOptN("opacity", svgFloat(a, "opacity", 1), 1)
	m.elements = append(m.elements, svgWrap(fmt.Sprintf("<ellipse%s/>", s), svgStr(a, "transform", "")))
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: fmt.Sprintf("Ellipse %s", id)}}}, nil
}

func (m *SVGToolManager) handleAddLine(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := svgGetArgs(req)
	id := m.svgGenID("l")
	s := svgAttr("id", id) + fmt.Sprintf(` x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"`, svgFloat(a, "x1", 0), svgFloat(a, "y1", 0), svgFloat(a, "x2", 100), svgFloat(a, "y2", 100)) +
		svgOpt("stroke", svgStr(a, "stroke", "#ffffff")) + svgOptN("stroke-width", svgFloat(a, "stroke_width", 1), 1) +
		svgOptN("opacity", svgFloat(a, "opacity", 1), 1)
	m.elements = append(m.elements, svgWrap(fmt.Sprintf("<line%s/>", s), svgStr(a, "transform", "")))
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: fmt.Sprintf("Line %s", id)}}}, nil
}

func (m *SVGToolManager) handleAddPolygon(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := svgGetArgs(req)
	pts := svgStr(a, "points", "")
	if pts == "" {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: "points is required"}}, IsError: true}, nil
	}
	id := m.svgGenID("pg")
	s := svgAttr("id", id) + svgAttr("points", pts) + svgOpt("fill", svgStr(a, "fill", "#ffffff")) +
		svgOpt("stroke", svgStr(a, "stroke", "")) + svgOptN("stroke-width", svgFloat(a, "stroke_width", 0), 0) +
		svgOptN("opacity", svgFloat(a, "opacity", 1), 1)
	m.elements = append(m.elements, svgWrap(fmt.Sprintf("<polygon%s/>", s), svgStr(a, "transform", "")))
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: fmt.Sprintf("Polygon %s", id)}}}, nil
}

func (m *SVGToolManager) handleAddPath(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := svgGetArgs(req)
	d := svgStr(a, "d", "")
	if d == "" {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: "d (path data) is required"}}, IsError: true}, nil
	}
	id := m.svgGenID("p")
	s := svgAttr("id", id) + svgAttr("d", d) + svgOpt("fill", svgStr(a, "fill", "none")) +
		svgOpt("stroke", svgStr(a, "stroke", "#ffffff")) + svgOptN("stroke-width", svgFloat(a, "stroke_width", 1), 1) +
		svgOptN("opacity", svgFloat(a, "opacity", 1), 1)
	m.elements = append(m.elements, svgWrap(fmt.Sprintf("<path%s/>", s), svgStr(a, "transform", "")))
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: fmt.Sprintf("Path %s", id)}}}, nil
}

func (m *SVGToolManager) handleAddText(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := svgGetArgs(req)
	text := svgStr(a, "text", "")
	if text == "" {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: "text is required"}}, IsError: true}, nil
	}
	id := m.svgGenID("t")
	s := svgAttr("id", id) + fmt.Sprintf(` x="%.1f" y="%.1f"`, svgFloat(a, "x", 0), svgFloat(a, "y", 0)) +
		svgAttr("font-size", fmt.Sprintf("%.1f", svgFloat(a, "font_size", 16))) +
		svgOpt("fill", svgStr(a, "fill", "#ffffff")) + svgOpt("font-weight", svgStr(a, "font_weight", "normal")) +
		svgOpt("font-family", svgStr(a, "font_family", "sans-serif")) + svgOpt("text-anchor", svgStr(a, "text_anchor", "start")) +
		svgOptN("opacity", svgFloat(a, "opacity", 1), 1)
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
	m.elements = append(m.elements, svgWrap(fmt.Sprintf("<text%s>%s</text>", s, esc), svgStr(a, "transform", "")))
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: fmt.Sprintf("Text %s '%s'", id, text)}}}, nil
}

func (m *SVGToolManager) handleAddLinearGradient(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := svgGetArgs(req)
	id := svgStr(a, "id", "")
	if id == "" {
		id = m.svgGenID("lg")
	}
	stopsStr := svgStr(a, "stops", "")
	if stopsStr == "" {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: "stops is required"}}, IsError: true}, nil
	}
	stops := svgParseStops(stopsStr)
	if len(stops) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: "Failed to parse stops"}}, IsError: true}, nil
	}
	xml := fmt.Sprintf("  <linearGradient id=\"%s\" x1=\"%s\" y1=\"%s\" x2=\"%s\" y2=\"%s\">\n%s  </linearGradient>",
		id, svgStr(a, "x1", "0%"), svgStr(a, "y1", "0%"), svgStr(a, "x2", "100%"), svgStr(a, "y2", "0%"), svgStopsXML(stops))
	m.defs = append(m.defs, xml)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: fmt.Sprintf("Linear gradient id=%s", id)}}}, nil
}

func (m *SVGToolManager) handleAddRadialGradient(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := svgGetArgs(req)
	id := svgStr(a, "id", "")
	if id == "" {
		id = m.svgGenID("rg")
	}
	stopsStr := svgStr(a, "stops", "")
	if stopsStr == "" {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: "stops is required"}}, IsError: true}, nil
	}
	stops := svgParseStops(stopsStr)
	if len(stops) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: "Failed to parse stops"}}, IsError: true}, nil
	}
	xml := fmt.Sprintf("  <radialGradient id=\"%s\" cx=\"%s\" cy=\"%s\" r=\"%s\">\n%s  </radialGradient>",
		id, svgStr(a, "cx", "50%"), svgStr(a, "cy", "50%"), svgStr(a, "r", "50%"), svgStopsXML(stops))
	m.defs = append(m.defs, xml)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: fmt.Sprintf("Radial gradient id=%s", id)}}}, nil
}

func (m *SVGToolManager) handleAddArc(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := svgGetArgs(req)
	cx, cy, r := svgFloat(a, "cx", 0), svgFloat(a, "cy", 0), svgFloat(a, "r", 50)
	sd, ed := svgFloat(a, "start_angle", 0), svgFloat(a, "end_angle", 180)
	sr, er := sd*math.Pi/180, ed*math.Pi/180
	x1, y1 := cx+r*math.Cos(sr), cy+r*math.Sin(sr)
	x2, y2 := cx+r*math.Cos(er), cy+r*math.Sin(er)
	sweep := ed - sd
	if sweep < 0 {
		sweep += 360
	}
	la := 0
	if sweep > 180 {
		la = 1
	}
	d := fmt.Sprintf("M %.2f %.2f A %.2f %.2f 0 %d 1 %.2f %.2f", x1, y1, r, r, la, x2, y2)
	id := m.svgGenID("arc")
	s := svgAttr("id", id) + svgAttr("d", d) + svgOpt("fill", svgStr(a, "fill", "none")) +
		svgOpt("stroke", svgStr(a, "stroke", "#ffffff")) + svgOptN("stroke-width", svgFloat(a, "stroke_width", 2), 2) +
		svgOptN("opacity", svgFloat(a, "opacity", 1), 1)
	m.elements = append(m.elements, svgWrap(fmt.Sprintf("<path%s/>", s), svgStr(a, "transform", "")))
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: fmt.Sprintf("Arc %s", id)}}}, nil
}

func (m *SVGToolManager) handleAddStar(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := svgGetArgs(req)
	cx, cy := svgFloat(a, "cx", 0), svgFloat(a, "cy", 0)
	oR, iR := svgFloat(a, "outer_r", 50), svgFloat(a, "inner_r", 25)
	np := int(svgFloat(a, "points", 5))
	rot := svgFloat(a, "rotation", -90)
	if np < 3 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: "points must be >= 3"}}, IsError: true}, nil
	}
	t := np * 2
	pts := make([]string, t)
	for i := 0; i < t; i++ {
		angle := (float64(i)*360/float64(t) + rot) * math.Pi / 180
		r := oR
		if i%2 == 1 {
			r = iR
		}
		pts[i] = fmt.Sprintf("%.1f,%.1f", cx+r*math.Cos(angle), cy+r*math.Sin(angle))
	}
	id := m.svgGenID("star")
	s := svgAttr("id", id) + svgAttr("points", strings.Join(pts, " ")) + svgOpt("fill", svgStr(a, "fill", "#ffffff")) +
		svgOpt("stroke", svgStr(a, "stroke", "")) + svgOptN("stroke-width", svgFloat(a, "stroke_width", 0), 0) +
		svgOptN("opacity", svgFloat(a, "opacity", 1), 1)
	m.elements = append(m.elements, svgWrap(fmt.Sprintf("<polygon%s/>", s), svgStr(a, "transform", "")))
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: fmt.Sprintf("Star %s %dpt", id, np)}}}, nil
}

func (m *SVGToolManager) handleExport(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.elements) == 0 && len(m.defs) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: "Nothing to export"}}, IsError: true}, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`, m.width, m.height, m.width, m.height))
	sb.WriteString("\n")
	if m.bgColor != "" {
		sb.WriteString(fmt.Sprintf(`  <rect width="100%%" height="100%%" fill="%s"/>`, m.bgColor))
		sb.WriteString("\n")
	}
	if len(m.defs) > 0 {
		sb.WriteString("  <defs>\n")
		for _, d := range m.defs {
			sb.WriteString(d + "\n")
		}
		sb.WriteString("  </defs>\n")
	}
	for _, e := range m.elements {
		sb.WriteString(e + "\n")
	}
	sb.WriteString("</svg>")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: sb.String()}}}, nil
}

func (m *SVGToolManager) handleClear(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.elements, m.defs, m.nextID, m.bgColor = nil, nil, 0, ""
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Type: "text", Text: "SVG state cleared"}}}, nil
}

// ================================================================
// REGISTRATION
// ================================================================

func RegisterSVGTools(s *server.MCPServer, m *SVGToolManager) {
	s.AddTool(mcp.NewTool("svg_create_canvas",
		mcp.WithDescription("Initialize SVG canvas. MUST be called first. Resets state."),
		mcp.WithNumber("width", mcp.Description("Width in pixels.")),
		mcp.WithNumber("height", mcp.Description("Height in pixels.")),
		mcp.WithString("bg_color", mcp.Description("Background color. Default: transparent.")),
	), m.handleCreateCanvas)

	s.AddTool(mcp.NewTool("svg_add_circle",
		mcp.WithDescription("Add a circle."),
		mcp.WithNumber("cx", mcp.Description("Center X.")), mcp.WithNumber("cy", mcp.Description("Center Y.")),
		mcp.WithNumber("r", mcp.Description("Radius.")),
		mcp.WithString("fill", mcp.Description("Fill color. Default: #ffffff")),
		mcp.WithString("stroke", mcp.Description("Stroke color.")),
		mcp.WithNumber("stroke_width", mcp.Description("Stroke width. Default: 0.")),
		mcp.WithNumber("opacity", mcp.Description("0-1. Default: 1.")),
		mcp.WithString("transform", mcp.Description("SVG transform.")),
	), m.handleAddCircle)

	s.AddTool(mcp.NewTool("svg_add_rect",
		mcp.WithDescription("Add a rectangle."),
		mcp.WithNumber("x", mcp.Description("X.")), mcp.WithNumber("y", mcp.Description("Y.")),
		mcp.WithNumber("width", mcp.Description("Width.")), mcp.WithNumber("height", mcp.Description("Height.")),
		mcp.WithString("fill", mcp.Description("Fill color. Default: #ffffff")),
		mcp.WithString("stroke", mcp.Description("Stroke color.")),
		mcp.WithNumber("stroke_width", mcp.Description("Stroke width. Default: 0.")),
		mcp.WithNumber("rx", mcp.Description("Corner radius X.")),
		mcp.WithNumber("ry", mcp.Description("Corner radius Y.")),
		mcp.WithNumber("opacity", mcp.Description("0-1. Default: 1.")),
		mcp.WithString("transform", mcp.Description("SVG transform.")),
	), m.handleAddRect)

	s.AddTool(mcp.NewTool("svg_add_ellipse",
		mcp.WithDescription("Add an ellipse."),
		mcp.WithNumber("cx", mcp.Description("Center X.")), mcp.WithNumber("cy", mcp.Description("Center Y.")),
		mcp.WithNumber("rx", mcp.Description("Radius X.")), mcp.WithNumber("ry", mcp.Description("Radius Y.")),
		mcp.WithString("fill", mcp.Description("Fill color. Default: #ffffff")),
		mcp.WithString("stroke", mcp.Description("Stroke color.")),
		mcp.WithNumber("stroke_width", mcp.Description("Stroke width.")),
		mcp.WithNumber("opacity", mcp.Description("0-1. Default: 1.")),
		mcp.WithString("transform", mcp.Description("SVG transform.")),
	), m.handleAddEllipse)

	s.AddTool(mcp.NewTool("svg_add_line",
		mcp.WithDescription("Add a line."),
		mcp.WithNumber("x1", mcp.Description("Start X.")), mcp.WithNumber("y1", mcp.Description("Start Y.")),
		mcp.WithNumber("x2", mcp.Description("End X.")), mcp.WithNumber("y2", mcp.Description("End Y.")),
		mcp.WithString("stroke", mcp.Description("Stroke color. Default: #ffffff")),
		mcp.WithNumber("stroke_width", mcp.Description("Stroke width. Default: 1.")),
		mcp.WithNumber("opacity", mcp.Description("0-1. Default: 1.")),
		mcp.WithString("transform", mcp.Description("SVG transform.")),
	), m.handleAddLine)

	s.AddTool(mcp.NewTool("svg_add_polygon",
		mcp.WithDescription("Add polygon. Format: 'x1,y1 x2,y2'. Use svg_add_star for stars."),
		mcp.WithString("points", mcp.Description("Points string.")),
		mcp.WithString("fill", mcp.Description("Fill. Default: #ffffff")),
		mcp.WithString("stroke", mcp.Description("Stroke.")),
		mcp.WithNumber("stroke_width", mcp.Description("Width.")),
		mcp.WithNumber("opacity", mcp.Description("0-1.")),
		mcp.WithString("transform", mcp.Description("Transform.")),
	), m.handleAddPolygon)

	s.AddTool(mcp.NewTool("svg_add_path",
		mcp.WithDescription("Add path. For arcs use svg_add_arc."),
		mcp.WithString("d", mcp.Description("Path data.")),
		mcp.WithString("fill", mcp.Description("Fill. Default: none.")),
		mcp.WithString("stroke", mcp.Description("Stroke. Default: #ffffff")),
		mcp.WithNumber("stroke_width", mcp.Description("Width.")),
		mcp.WithNumber("opacity", mcp.Description("0-1.")),
		mcp.WithString("transform", mcp.Description("Transform.")),
	), m.handleAddPath)

	s.AddTool(mcp.NewTool("svg_add_text",
		mcp.WithDescription("Add text."),
		mcp.WithNumber("x", mcp.Description("X.")), mcp.WithNumber("y", mcp.Description("Y.")),
		mcp.WithString("text", mcp.Description("Content.")),
		mcp.WithNumber("font_size", mcp.Description("Size. Default: 16.")),
		mcp.WithString("fill", mcp.Description("Color. Default: #ffffff")),
		mcp.WithString("font_weight", mcp.Description("Weight.")),
		mcp.WithString("font_family", mcp.Description("Family.")),
		mcp.WithString("text_anchor", mcp.Description("start/middle/end.")),
		mcp.WithNumber("opacity", mcp.Description("0-1.")),
		mcp.WithString("transform", mcp.Description("Transform.")),
	), m.handleAddText)

	s.AddTool(mcp.NewTool("svg_add_linear_gradient",
		mcp.WithDescription("Add linear gradient. Stops: '0%:#ff0000, 100%:#0000ff'. Use fill=\"url(#id)\"."),
		mcp.WithString("id", mcp.Description("ID.")),
		mcp.WithString("x1", mcp.Description("Default: 0%.")), mcp.WithString("y1", mcp.Description("Default: 0%.")),
		mcp.WithString("x2", mcp.Description("Default: 100%.")), mcp.WithString("y2", mcp.Description("Default: 0%.")),
		mcp.WithString("stops", mcp.Description("Color stops.")),
	), m.handleAddLinearGradient)

	s.AddTool(mcp.NewTool("svg_add_radial_gradient",
		mcp.WithDescription("Add radial gradient. Stops: '0%:#fff, 100%:#000'. Use fill=\"url(#id)\"."),
		mcp.WithString("id", mcp.Description("ID.")),
		mcp.WithString("cx", mcp.Description("Default: 50%.")), mcp.WithString("cy", mcp.Description("Default: 50%.")),
		mcp.WithString("r", mcp.Description("Default: 50%.")),
		mcp.WithString("stops", mcp.Description("Color stops.")),
	), m.handleAddRadialGradient)

	s.AddTool(mcp.NewTool("svg_add_arc",
		mcp.WithDescription("Add arc. Computes SVG A-command math. Angles: 0=right, 90=down."),
		mcp.WithNumber("cx", mcp.Description("Center X.")), mcp.WithNumber("cy", mcp.Description("Center Y.")),
		mcp.WithNumber("r", mcp.Description("Radius.")),
		mcp.WithNumber("start_angle", mcp.Description("Degrees.")),
		mcp.WithNumber("end_angle", mcp.Description("Degrees.")),
		mcp.WithString("fill", mcp.Description("Fill. Default: none.")),
		mcp.WithString("stroke", mcp.Description("Stroke. Default: #ffffff")),
		mcp.WithNumber("stroke_width", mcp.Description("Width.")),
		mcp.WithNumber("opacity", mcp.Description("0-1.")),
		mcp.WithString("transform", mcp.Description("Transform.")),
	), m.handleAddArc)

	s.AddTool(mcp.NewTool("svg_add_star",
		mcp.WithDescription("Add star. Computes vertex math. Default points up."),
		mcp.WithNumber("cx", mcp.Description("Center X.")), mcp.WithNumber("cy", mcp.Description("Center Y.")),
		mcp.WithNumber("outer_r", mcp.Description("Outer radius.")),
		mcp.WithNumber("inner_r", mcp.Description("Inner radius.")),
		mcp.WithNumber("points", mcp.Description("Point count. Default: 5.")),
		mcp.WithNumber("rotation", mcp.Description("Degrees. Default: -90.")),
		mcp.WithString("fill", mcp.Description("Fill.")),
		mcp.WithString("stroke", mcp.Description("Stroke.")),
		mcp.WithNumber("stroke_width", mcp.Description("Width.")),
		mcp.WithNumber("opacity", mcp.Description("0-1.")),
		mcp.WithString("transform", mcp.Description("Transform.")),
	), m.handleAddStar)

	s.AddTool(mcp.NewTool("svg_export",
		mcp.WithDescription("Export all elements to complete SVG string. Call LAST."),
	), m.handleExport)

	s.AddTool(mcp.NewTool("svg_clear",
		mcp.WithDescription("Reset all SVG state."),
	), m.handleClear)
}
