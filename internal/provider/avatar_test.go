package provider

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/serving"
)

// roundTripFunc 让测试不依赖真实网络：头像的 hostname 白名单必须保留（那是安全边界），
// 所以用自定义 Transport 在"已经通过校验的真实 URL"上直接给响应。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func response(status int, contentType string, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

const repoPayloadWithAvatar = `{"id":7,"full_name":"o/r","private":false,"visibility":"public",` +
	`"stargazers_count":3,"created_at":"2026-01-01T00:00:00Z",` +
	`"owner":{"avatar_url":"https://avatars.githubusercontent.com/u/42?v=4"}}`

func avatarFixture(t *testing.T) (*serving.Store, *[]string, *GitHubProvider) {
	t.Helper()
	store, err := serving.Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seen := &[]string{}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.EqualFold(request.URL.Hostname(), "avatars.githubusercontent.com") {
			*seen = append(*seen, request.URL.String())
			return response(http.StatusOK, "image/png", bytes.Repeat([]byte{0x89}, 512)), nil
		}
		return response(http.StatusOK, "application/json", []byte(repoPayloadWithAvatar)), nil
	})}
	provider := NewGitHubProvider("https://api.github.com", "token", client).WithAvatarCache(store)
	provider.now = func() time.Time { return time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC) }
	return store, seen, provider
}

// 头像必须请求小尺寸变体：完整头像是 297KB，卡片只渲染到 ~96px，原图纯属浪费带宽，
// 而且 8s 的客户端超时会被它吃满。
func TestAvatarRequestsSizedVariant(t *testing.T) {
	_, seen, provider := avatarFixture(t)
	metadata, err := provider.Fetch(t.Context(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected exactly one avatar request, got %d: %v", len(*seen), *seen)
	}
	if !strings.Contains((*seen)[0], "s=128") {
		t.Fatalf("avatar request must carry the size parameter, got %s", (*seen)[0])
	}
	if !strings.HasPrefix(metadata.AvatarDataURI, "data:image/png;base64,") {
		t.Fatalf("unexpected avatar data URI: %q", metadata.AvatarDataURI)
	}
}

// 头像按 URL 缓存：同一个仓库的第二次元数据刷新不该再下载一遍头像。
func TestAvatarIsReusedFromCache(t *testing.T) {
	store, seen, provider := avatarFixture(t)
	first, err := provider.Fetch(t.Context(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Fetch(t.Context(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 {
		t.Fatalf("second fetch must reuse the cached avatar, avatar requests=%d", len(*seen))
	}
	if first.AvatarDataURI != second.AvatarDataURI {
		t.Fatal("cached avatar differs from the freshly fetched one")
	}
	// 缓存键是带尺寸参数的完整 URL，保证"换了尺寸就换一份缓存"。
	if _, _, found, err := store.Avatar(t.Context(), (*seen)[0]); err != nil || !found {
		t.Fatalf("avatar cache row missing: found=%v err=%v", found, err)
	}
}

// 超过上限的头像必须被拒绝，让卡片回退到首字母占位，而不是把 400KB 塞进 README 图片。
func TestOversizedAvatarIsRejected(t *testing.T) {
	store, err := serving.Open(t.TempDir() + "/history.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.EqualFold(request.URL.Hostname(), "avatars.githubusercontent.com") {
			return response(http.StatusOK, "image/png", bytes.Repeat([]byte{0x89}, maximumAvatarBytes+1)), nil
		}
		return response(http.StatusOK, "application/json", []byte(repoPayloadWithAvatar)), nil
	})}
	provider := NewGitHubProvider("https://api.github.com", "token", client).WithAvatarCache(store)
	if metadata, err := provider.Fetch(t.Context(), "o", "r"); err != nil {
		t.Fatal(err)
	} else if metadata.AvatarDataURI != "" {
		t.Fatalf("oversized avatar must be dropped, got %d bytes", len(metadata.AvatarDataURI))
	}
}
