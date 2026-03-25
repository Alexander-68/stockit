package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
)

//go:embed templates/*.gohtml assets/**/*
var embeddedFiles embed.FS

type Templates struct {
	tpl *template.Template
}

func NewTemplates() (*Templates, error) {
	tpl, err := template.New("base").Funcs(template.FuncMap{
		"eq": func(left, right any) bool { return left == right },
	}).ParseFS(embeddedFiles, "templates/*.gohtml")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Templates{tpl: tpl}, nil
}

func (t *Templates) Render(w http.ResponseWriter, name string, data any) error {
	return t.tpl.ExecuteTemplate(w, name, data)
}

func AssetFS() http.FileSystem {
	assetsFS, err := fs.Sub(embeddedFiles, "assets")
	if err != nil {
		panic(err)
	}
	return http.FS(assetsFS)
}
