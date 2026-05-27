package main

import (
	"html/template"
	"log"
)

func loadTemplates() (index *template.Template, paste *template.Template, adminLogin *template.Template, adminDashboard *template.Template) {
	var err error

	index, err = template.ParseFiles("templates/index.html")
	if err != nil {
		index = template.Must(template.New("index").Parse(fallbackIndexHTML))
	} else {
		log.Println("templates/index.html loaded from disk")
	}

	paste, err = template.ParseFiles("templates/paste.html")
	if err != nil {
		paste = template.Must(template.New("paste").Parse(fallbackPasteHTML))
	} else {
		log.Println("templates/paste.html loaded from disk")
	}

	adminLogin, err = template.ParseFiles("templates/admin_login.html")
	if err != nil {
		adminLogin = template.Must(template.New("admin_login").Parse(fallbackAdminLoginHTML))
	} else {
		log.Println("templates/admin_login.html loaded from disk")
	}

	adminDashboard, err = template.ParseFiles("templates/admin_dashboard.html")
	if err != nil {
		adminDashboard = template.Must(template.New("admin_dashboard").Parse(fallbackAdminDashboardHTML))
	} else {
		log.Println("templates/admin_dashboard.html loaded from disk")
	}

	return
}
