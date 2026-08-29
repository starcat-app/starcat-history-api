// Package middleware 收敛 History API 的 HTTP 中间件。
package middleware

import (
	"net/http"

	kitauth "github.com/starcat-app/starcat-api-kit/auth"
	kitcors "github.com/starcat-app/starcat-api-kit/cors"
)

type BearerAuth = kitauth.BearerAuth

func NewBearerAuth(keys []string) *BearerAuth { return kitauth.NewBearerAuth(keys) }

func CORS(next http.Handler) http.Handler { return kitcors.Handler(next) }
