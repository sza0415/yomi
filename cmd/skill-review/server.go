package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ziangsun/szabot/internal/skillreview"
)

//go:embed web/index.html
var dashboardFS embed.FS

type dashboardData struct {
	Modules []skillreview.ReviewModule   `json:"modules"`
	Report  skillreview.Report           `json:"report"`
	Paths   []skillreview.PathDefinition `json:"paths"`
}

func serveDashboard(addr string, report skillreview.Report, paths []skillreview.PathDefinition, skillsAPI *skillsAPI) error {
	data, err := json.Marshal(dashboardData{Modules: skillreview.ReviewModules(), Report: report, Paths: paths})
	if err != nil {
		return fmt.Errorf("marshal dashboard data: %w", err)
	}
	mux := http.NewServeMux()
	if skillsAPI != nil {
		skillsAPI.register(mux)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page, readErr := dashboardFS.ReadFile("web/index.html")
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(page)
	})
	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(data)
	})

	url := "http://localhost" + addr
	fmt.Printf("skill-review dashboard running at %s\n", url)
	return http.ListenAndServe(addr, mux)
}
