package main

import (
	"embed"
	"html/template"
	"log"
)

//go:embed templates/*.html
var embeddedTemplates embed.FS

func loadTemplates() (index *template.Template, paste *template.Template, password *template.Template, adminLogin *template.Template, adminDashboard *template.Template) {
	index = loadEmbeddedTemplate("index.html")
	paste = loadEmbeddedTemplate("paste.html")
	password = loadEmbeddedTemplate("password.html")
	adminLogin = loadEmbeddedTemplate("admin_login.html")
	adminDashboard = loadEmbeddedTemplate("admin_dashboard.html")
	return
}

func loadEmbeddedTemplate(name string) *template.Template {
	data, err := embeddedTemplates.ReadFile("templates/" + name)
	if err != nil {
		log.Fatalf("내장 템플릿 %s을 찾을 수 없음: %v", name, err)
	}
	return template.Must(template.New(name).Parse(string(data)))
}
