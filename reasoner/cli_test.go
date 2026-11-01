package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cli_test.go 用真实子进程守住 -check 的契约：只打一次上游，不启动服务、
// 不连中心、不写秘密文件；缺 Key 时明确失败且不请求上游。

type checkUpstream struct {
	srv      *httptest.Server
	requests atomic.Int64
	mu       sync.Mutex
	auth     []string
}

func newCheckUpstream(t *testing.T) *checkUpstream {
	t.Helper()
	u := &checkUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.requests.Add(1)
		u.mu.Lock()
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.mu.Unlock()

		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		prompt := ""
		if len(req.Messages) > 0 {
			prompt = req.Messages[len(req.Messages)-1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]string{"content": "答:" + prompt},
				"finish_reason": "stop",
			}},
		})
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func TestCheckSubprocessDoesNotStartService(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "reasoner-check")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	buildCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建测试程序失败: %v\n%s", err, out)
	}

	fq := newCheckUpstream(t)
	run := func(dir string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	baseArgs := func() []string {
		return []string{
			"-check",
			"-llm-base", fq.srv.URL,
			// 故意给一个连不上的中心：自检一旦尝试注册就会失败。
			"-center", "ws://127.0.0.1:1/ws",
		}
	}

	t.Run("成功自检只打一次上游", func(t *testing.T) {
		dir := t.TempDir()
		before := fq.requests.Load()
		// 模拟从剪贴板粘进来的 Key：首尾带空白和换行也必须能用。
		paddedKey := " \r\n" + testKey + "\n "
		args := append(baseArgs(), "-llm-key", paddedKey, "-check-prompt", "诊断自检")
		out, err := run(dir, args...)
		if err != nil {
			t.Fatalf("自检失败: %v\n%s", err, out)
		}
		if got := fq.requests.Load() - before; got != 1 {
			t.Fatalf("自检应只请求上游一次，实际 %d", got)
		}
		if !strings.Contains(out, "问：诊断自检") || !strings.Contains(out, "答（") ||
			!strings.Contains(out, "答:诊断自检") {
			t.Fatalf("自检输出异常:\n%s", out)
		}
		if strings.Contains(out, testKey) {
			t.Fatalf("自检输出不应包含 Key:\n%s", out)
		}
		fq.mu.Lock()
		auth := fq.auth[len(fq.auth)-1]
		fq.mu.Unlock()
		if auth != "Bearer "+testKey {
			t.Fatalf("自检未使用本次提供的 Key: %q", auth)
		}
		if _, err := os.Stat(filepath.Join(dir, "data")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("自检不应创建 data 目录: %v", err)
		}
	})

	t.Run("缺Key明确失败且不打上游", func(t *testing.T) {
		before := fq.requests.Load()
		out, err := run(t.TempDir(), baseArgs()...)
		if err == nil {
			t.Fatalf("缺 Key 应失败:\n%s", out)
		}
		if !strings.Contains(out, "自检需要 -llm-key") {
			t.Fatalf("缺 Key 错误不明确:\n%s", out)
		}
		if got := fq.requests.Load() - before; got != 0 {
			t.Fatalf("缺 Key 不应请求上游，实际 %d 次", got)
		}
	})
}
