package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Link 头是冷启动并行的唯一依据：解析错了就会退化成串行，或者把页数读错导致漏数据。
func TestParseLastPage(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   int
	}{
		{
			name:   "GitHub 的典型多页响应",
			header: `<https://api.github.com/repositories/1/stargazers/history?page=2>; rel="next", <https://api.github.com/repositories/1/stargazers/history?page=23>; rel="last"`,
			want:   23,
		},
		{
			name:   "只有 last",
			header: `<https://api.github.com/repositories/1/stargazers/history?page=7>; rel="last"`,
			want:   7,
		},
		{
			name:   "last 在 next 之前",
			header: `<https://api.github.com/repositories/1/stargazers/history?page=5>; rel="last", <https://api.github.com/repositories/1/stargazers/history?page=2>; rel="next"`,
			want:   5,
		},
		{
			name:   "单页响应没有 last",
			header: `<https://api.github.com/repositories/1/stargazers/history?page=1>; rel="first"`,
			want:   0,
		},
		{"空头", "", 0},
		{"无法解析", "garbage", 0},
		{"页码非法", `<https://api.github.com/repositories/1/stargazers/history?page=0>; rel="last"`, 0},
		{"缺少尖括号", `<https://api.github.com>; rel="last"`, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := parseLastPage(testCase.header); got != testCase.want {
				t.Fatalf("parseLastPage(%q) = %d, want %d", testCase.header, got, testCase.want)
			}
		})
	}
}

// StarHistory 必须把 Link 头带出来：handler 靠它决定并行拉取还是顺序拉取。
func TestStarHistoryExposesLastPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", `<https://api.github.com/repositories/1/stargazers/history?page=2>; rel="next", <https://api.github.com/repositories/1/stargazers/history?page=9>; rel="last"`)
		_, _ = w.Write([]byte(`[{"week":1725696000,"total":1,"days":[0,1,0,0,0,0,0]}]`))
	}))
	defer server.Close()
	provider := NewGitHubProvider(server.URL, "", server.Client())

	response, err := provider.StarHistory(t.Context(), "o", "r", 1, 30, "")
	if err != nil {
		t.Fatal(err)
	}
	if response.LastPage != 9 {
		t.Fatalf("expected LastPage 9, got %d", response.LastPage)
	}
}
