package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIfNoneMatchToleratesWeakPrefixAndLists(t *testing.T) {
	const etag = `"abc123"`
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"精确匹配", etag, true},
		{"nginx gzip 改写后的弱校验", `W/"abc123"`, true},
		{"弱校验带空格", `  W/"abc123"  `, true},
		{"逗号列表命中其中一项", `"other", W/"abc123"`, true},
		{"通配符", `*`, true},
		{"不匹配", `"other"`, false},
		{"空头", ``, false},
		{"只有前缀", `W/`, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ifNoneMatch(testCase.header, etag); got != testCase.want {
				t.Fatalf("ifNoneMatch(%q) = %v, want %v", testCase.header, got, testCase.want)
			}
		})
	}
}

// 嵌入响应经过 nginx gzip 之后，客户端会拿弱校验 ETag 回来；必须仍然拿到 304，
// 否则压缩省下的带宽会被"每次回传完整正文"抵消。
func TestEmbedSupportsWeakETagRevalidation(t *testing.T) {
	_, handler, _, _ := seedHistoryFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /embed/v1/repos/{owner}/{repo}/star-history.svg", handler.HandleStarHistoryEmbed)
	const path = "/embed/v1/repos/owner/repo/star-history.svg?theme=light&locale=en"

	call := func(ifNoneMatch string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if ifNoneMatch != "" {
			request.Header.Set("If-None-Match", ifNoneMatch)
		}
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		return recorder
	}

	first := call("")
	if first.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	if weak := call("W/" + etag); weak.Code != http.StatusNotModified {
		t.Fatalf("weak ETag revalidation must return 304, got %d", weak.Code)
	}
	if list := call(`"nope", W/` + etag); list.Code != http.StatusNotModified {
		t.Fatalf("ETag list revalidation must return 304, got %d", list.Code)
	}
	if stale := call(`"nope"`); stale.Code != http.StatusOK {
		t.Fatalf("non-matching ETag must return 200, got %d", stale.Code)
	}
}
