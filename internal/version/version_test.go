package version

import (
	"os"
	"testing"
)

// TestVersionMatchesExpectedBuildValue 同时约束开发默认值与发布时的 linker 注入。
// 如果 Version 被误改回 const，带 -ldflags -X 的 CI 用例会保留默认值并在这里失败。
func TestVersionMatchesExpectedBuildValue(t *testing.T) {
	expected := os.Getenv("STARCAT_HISTORY_EXPECTED_VERSION")
	if expected == "" {
		expected = "0.0.0-dev"
	}
	if Version != expected {
		t.Fatalf("Version = %q, want %q", Version, expected)
	}
}
