// Package web embeds the ChronoHost web portal assets.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"strings"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticRoot embed.FS

// Static returns the HTTP filesystem for /static/ assets.
func Static() (fs.FS, error) {
	return fs.Sub(staticRoot, "static")
}

// Templates parses all html/template files with the shared function map.
func Templates() (*template.Template, error) {
	static, err := Static()
	if err != nil {
		return nil, err
	}
	_ = static
	funcs := template.FuncMap{
		"prettyJSON": func(v any) string {
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				return fmt.Sprintf("%v", v)
			}
			return string(b)
		},
		"jsonJS": func(v any) template.JS {
			b, err := json.Marshal(v)
			if err != nil {
				return template.JS("null")
			}
			return template.JS(b)
		},
		"humanBytes": func(n int64) string {
			const unit = 1024
			if n < unit {
				return fmt.Sprintf("%d B", n)
			}
			div, exp := int64(unit), 0
			for v := n / unit; v >= unit; v /= unit {
				div *= unit
				exp++
			}
			return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
		},
		"timeAgo": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		},
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"formatTimePtr": func(t *time.Time) string {
			if t == nil || t.IsZero() {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"join": strings.Join,
		"initial": func(s string) string {
			if s == "" {
				return "?"
			}
			return strings.ToUpper(s[:1])
		},
		"default": func(v, fallback string) string {
			if v == "" {
				return fallback
			}
			return v
		},
		"lower":     strings.ToLower,
		"hasPrefix": strings.HasPrefix,
		"eqf":       func(a, b float64) bool { return a == b },
		"add":       func(a, b int) int { return a + b },
		"statusClass": func(s string) string {
			switch strings.ToLower(s) {
			case "running":
				return "ok"
			case "starting", "stopping":
				return "warn"
			case "crashed":
				return "bad"
			default:
				return "idle"
			}
		},
		"selected": func(current, value string) string {
			if current == value {
				return " selected"
			}
			return ""
		},
		"checked": func(v bool) string {
			if v {
				return " checked"
			}
			return ""
		},
	}
	return template.New("root").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
}
