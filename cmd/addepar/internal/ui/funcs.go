package ui

import (
	"html/template"
	"time"
)

// funcMap is the templates' functions.
func funcMap() template.FuncMap {
	return template.FuncMap{
		// when shows a time in the local zone.
		"when": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Local().Format("Jan 2, 2006 3:04 PM")
		},
	}
}
