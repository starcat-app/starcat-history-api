package handler

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/starcat-app/starcat-history-api/internal/model"
)

func writeJSON[T any](w http.ResponseWriter, status int, data T) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(model.DataEnvelope[T]{SchemaVersion: 1, Data: data}); err != nil {
		log.Printf("[handler] encode success response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string, details any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	value := model.ErrorEnvelope{SchemaVersion: 1, Error: model.ErrorResponse{Code: code, Message: message, Details: details}}
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("[handler] encode error response: %v", err)
	}
}
