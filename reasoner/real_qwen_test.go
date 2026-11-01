package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// real_qwen_test.go 是可手动开启的真实上游测试：
//
//	QWEN_API_KEY=sk-... go test ./reasoner -run '^TestRealQwen' -v
//
// 默认没有环境变量时全部跳过，CI 不会产生真实调用，也不会接触密钥。
// 测试失败信息也不直接打印上游错误原文，避免真实 Key 被回显到日志。
const realQwenTimeout = 30 * time.Second

func realQwenConfig(t *testing.T) (key, base string) {
	t.Helper()
	key = strings.TrimSpace(os.Getenv("QWEN_API_KEY"))
	if key == "" {
		t.Skip("未设置 QWEN_API_KEY，跳过真实千问测试")
	}
	base = strings.TrimSpace(os.Getenv("QWEN_BASE_URL"))
	if base == "" {
		base = defaultBaseURL
	}
	return key, base
}

// realQwenSafeError 用于失败日志：如果错误文本仍含密钥，就完全不打印原文。
func realQwenSafeError(err error, key string) string {
	if err == nil {
		return "<nil>"
	}
	msg := err.Error()
	trimmed := strings.TrimSpace(key)
	if strings.Contains(msg, key) || (trimmed != "" && strings.Contains(msg, trimmed)) {
		return "（错误文本包含密钥，已省略）"
	}
	return msg
}

func realQwenThink(t *testing.T, key, base, prompt string, maxTokens int) (string, string, error) {
	t.Helper()
	return realQwenThinkWithClient(t, newChatClient(), key, base, prompt, maxTokens)
}

func realQwenThinkWithClient(t *testing.T, cl *chatClient, key, base, prompt string, maxTokens int) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), realQwenTimeout)
	defer cancel()
	return cl.think(ctx, base, key, defaultModel, "", prompt, maxTokens)
}

// TestRealQwenBasic 只确认真实 Key、真实 Base URL、默认模型和响应解析能跑通。
func TestRealQwenBasic(t *testing.T) {
	key, base := realQwenConfig(t)

	text, finish, err := realQwenThink(t, key, base, "只回复两个字：正常", 16)
	if err != nil {
		t.Fatalf("真实千问调用失败: %s", realQwenSafeError(err, key))
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("真实千问返回了空文本")
	}
	if strings.Contains(text, key) {
		t.Fatal("真实千问回答里出现了密钥")
	}
	t.Logf("真实千问成功：finish_reason=%q 回答=%q", finish, text)
}

// TestRealQwenPaddedKey 复现真实剪贴板场景：Key 带首尾空白/换行也必须能调用。
func TestRealQwenPaddedKey(t *testing.T) {
	key, base := realQwenConfig(t)

	text, finish, err := realQwenThink(t, " \r\n"+key+"\n ", base, "只回复两个字：正常", 16)
	if err != nil {
		t.Fatalf("带首尾空白的真实 Key 调用失败: %s", realQwenSafeError(err, key))
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("带首尾空白的真实 Key 返回了空文本")
	}
	t.Logf("带空白 Key 真实调用成功：finish_reason=%q 回答=%q", finish, text)
}

// TestRealQwenBadModelUsesStructuredError 验证真实非 200 错误体仍能被翻译成
// 可读错误，且不会把 Key 带进错误文本。
func TestRealQwenBadModelUsesStructuredError(t *testing.T) {
	key, base := realQwenConfig(t)
	model := strings.TrimSpace(os.Getenv("QWEN_BAD_MODEL"))
	if model == "" {
		model = "qwen-model-that-does-not-exist-20261101"
	}

	ctx, cancel := context.WithTimeout(context.Background(), realQwenTimeout)
	defer cancel()
	_, _, err := newChatClient().think(ctx, base, key, model, "", "只回复：ok", 8)
	if err == nil {
		t.Fatal("不存在的模型应返回错误")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatal("真实错误文本泄露了密钥")
	}
	if !strings.Contains(err.Error(), "HTTP") {
		t.Fatalf("错误应带 HTTP 状态码，实际: %s", realQwenSafeError(err, key))
	}
	t.Logf("真实错误（已脱敏）: %s", realQwenSafeError(err, key))
}

// TestRealQwenBadKeyDoesNotLeak 用一把故意改坏的 Key 触发上游鉴权错误，
// 检查真实错误体不会把 Key 带进最终错误文本。
func TestRealQwenBadKeyDoesNotLeak(t *testing.T) {
	key, base := realQwenConfig(t)
	badKey := key + "-invalid"

	ctx, cancel := context.WithTimeout(context.Background(), realQwenTimeout)
	defer cancel()
	_, _, err := newChatClient().think(ctx, base, badKey, defaultModel, "", "只回复：ok", 8)
	if err == nil {
		t.Fatal("改坏的 Key 应返回错误")
	}
	if strings.Contains(err.Error(), badKey) {
		t.Fatal("真实错误文本泄露了改坏的 Key")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatal("真实错误文本泄露了原始 Key")
	}
	t.Logf("真实鉴权错误（已脱敏）: %s", realQwenSafeError(err, key))
}

// TestRealQwenConcurrentIsolation 用同一个客户端并发发两个不同问题，
// 检查响应不会串线。只发两个请求，控制真实额度和限流影响。
func TestRealQwenConcurrentIsolation(t *testing.T) {
	key, base := realQwenConfig(t)

	prompts := []string{"只回复：苹果", "只回复：香蕉"}
	expected := []string{"苹果", "香蕉"}
	type result struct {
		text string
		err  error
	}
	results := make([]result, len(prompts))
	cl := newChatClient()
	var wg sync.WaitGroup
	for i, prompt := range prompts {
		wg.Add(1)
		go func(i int, prompt string) {
			defer wg.Done()
			results[i].text, _, results[i].err = realQwenThinkWithClient(t, cl, key, base, prompt, 8)
		}(i, prompt)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("并发请求 %d 失败: %s", i, realQwenSafeError(r.err, key))
		}
		if !strings.Contains(r.text, expected[i]) {
			t.Fatalf("并发请求 %d 可能串答：回答=%q", i, r.text)
		}
	}
}
