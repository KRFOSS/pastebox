package main

import (
	"embed"
	"html/template"
	"log"
	"os"
)

//go:embed templates/*.html
var embeddedTemplates embed.FS

func loadTemplates() (index *template.Template, paste *template.Template, password *template.Template, adminLogin *template.Template, adminDashboard *template.Template) {
	index = loadOneTemplate("index.html", "templates/index.html")
	paste = loadOneTemplate("paste.html", "templates/paste.html")
	password = loadOneTemplate("password.html", "templates/password.html")
	adminLogin = loadOneTemplate("admin_login.html", "templates/admin_login.html")
	adminDashboard = loadOneTemplate("admin_dashboard.html", "templates/admin_dashboard.html")
	return
}

func loadOneTemplate(embedName string, diskPath string) *template.Template {
	diskData, diskErr := os.ReadFile(diskPath)
	if diskErr == nil {
		log.Printf("%s loaded from disk", diskPath)
		return template.Must(template.New(embedName).Parse(string(diskData)))
	}

	embedData, err := embeddedTemplates.ReadFile("templates/" + embedName)
	if err != nil {
		log.Fatalf("embedded template %s not found: %v", embedName, err)
	}

	log.Printf("%s loaded from embedded fallback", embedName)
	return template.Must(template.New(embedName).Parse(string(embedData)))
}
