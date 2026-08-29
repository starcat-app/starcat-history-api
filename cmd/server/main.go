// Package main 启动独立 Star History API。
package main

import (
	"log"
	"net/http"

	"github.com/joho/godotenv"
	"github.com/starcat-app/starcat-history-api/server"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("[env] no .env file found, using OS environment only")
	}
	service, err := server.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	defer service.Close()
	log.Printf("starcat-history-api listening on %s", service.Addr())
	log.Fatal(http.ListenAndServe(service.Addr(), service.Handler()))
}
