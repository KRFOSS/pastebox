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
		log.Printf("%s 디스크에서 로드됨", diskPath)
		return template.Must(template.New(embedName).Parse(string(diskData)))
	}

	embedData, err := embeddedTemplates.ReadFile("templates/" + embedName)
	if err != nil {
		log.Fatalf("내장 템플릿 %s을 찾을 수 없음: %v", embedName, err)
	}

	log.Printf("%s 내장 템플릿(폴백)에서 로드됨", embedName)
	return template.Must(template.New(embedName).Parse(string(embedData)))
}
