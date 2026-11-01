package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// llm_test.go 只覆盖直接 HTTP 边界：响应体上限与上游错误脱敏。
// 服务层是否把错误原样带出去，由 main_test.go 的端到端用例守着。

type llmTestRoundTrip func(*http.Request) (*http.Response, error)

func (f llmTestRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type llmTestBody struct {
	r      *strings.Reader
	read   *atomic.Int64
	closed *atomic.Int64
}

func (b *llmTestBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *llmTestBody) Close() error {
	b.closed.Add(1)
	return nil
}

func llmTestClient(t *testing.T, status int, body string) (*chatClient, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var read, closed atomic.Int64
	return &chatClient{hc: &http.Client{Transport: llmTestRoundTrip(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       &llmTestBody{r: strings.NewReader(body), read: &read, closed: &closed},
			Request:    req,
		}, nil
	})}}, &read, &closed
}

func callLLMTest(t *testing.T, cl *chatClient, key string) (string, error) {
	t.Helper()
	text, _, err := cl.think(context.Background(), "http://llm-test.invalid", key, defaultModel, "", "问", 16)
	return text, err
}

// TestThinkResponseBodyLimit 守住“最多读 1MiB、超限明确报错”的边界。
func TestThinkResponseBodyLimit(t *testing.T) {
	const valid = `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`

	t.Run("小于上限正常", func(t *testing.T) {
		cl, read, closed := llmTestClient(t, http.StatusOK, valid)
		text, err := callLLMTest(t, cl, testKey)
		if err != nil || text != "ok" {
			t.Fatalf("对照组: text=%q err=%v", text, err)
		}
		if read.Load() != int64(len(valid)) || closed.Load() != 1 {
			t.Fatalf("读/关次数不对: read=%d closed=%d", read.Load(), closed.Load())
		}
	})

	t.Run("恰好到上限正常", func(t *testing.T) {
		body := valid + strings.Repeat(" ", maxBodyBytes-len(valid))
		cl, read, closed := llmTestClient(t, http.StatusOK, body)
		if _, err := callLLMTest(t, cl, testKey); err != nil {
			t.Fatalf("恰好等于上限不该被拒: %v", err)
		}
		if read.Load() != int64(maxBodyBytes) || closed.Load() != 1 {
			t.Fatalf("读/关次数不对: read=%d closed=%d", read.Load(), closed.Load())
		}
	})

	oversized := []struct {
		name string
		body string
	}{
		{"超限截断成非法JSON", `{"choices":[{"message":{"content":"` + strings.Repeat("x", maxBodyBytes) + `"},"finish_reason":"stop"}]}`},
		{"超限但前缀是完整JSON", valid + strings.Repeat(" ", maxBodyBytes+1-len(valid))},
	}
	for _, tc := range oversized {
		t.Run(tc.name, func(t *testing.T) {
			cl, read, closed := llmTestClient(t, http.StatusOK, tc.body)
			_, err := callLLMTest(t, cl, testKey)
			if err == nil {
				t.Fatal("超限响应应返回错误，不能静默接受")
			}
			for _, want := range []string{"超过", "字节上限"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("超限错误应明确包含 %q，实际 %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "不是合法 JSON") {
				t.Fatalf("超限不能被误报成 JSON 格式错误: %v", err)
			}
			if read.Load() != int64(maxBodyBytes)+1 {
				t.Fatalf("应只多读 1 字节用于判定超限，实际 read=%d", read.Load())
			}
			if closed.Load() != 1 {
				t.Fatalf("响应体应关闭且只关一次，实际 %d", closed.Load())
			}
		})
	}
}

// TestThinkNormalizesKeyWhitespace 守住剪贴板常见问题：Key 带了首尾空白或
// 换行时，Authorization 必须用清理后的值；换行原样进请求头会被 net/http 拒绝。
func TestThinkNormalizesKeyWhitespace(t *testing.T) {
	const valid = `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	var gotAuth string
	cl := &chatClient{hc: &http.Client{Transport: llmTestRoundTrip(func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(valid)),
			Request:    req,
		}, nil
	})}}

	text, _, err := cl.think(context.Background(), "http://llm-test.invalid", " \r\n"+testKey+"\n ", defaultModel, "", "问", 16)
	if err != nil || text != "ok" {
		t.Fatalf("带空白的 Key 应被归一化后正常使用: text=%q err=%v", text, err)
	}
	if gotAuth != "Bearer "+testKey {
		t.Fatalf("Authorization 应使用清理后的 Key，实际 %q", gotAuth)
	}
}

// TestUpstreamErrorRedactsWhitespacePaddedKey 防止“发的是清理后的 Key，
// 脱敏却拿原始带空白 Key 去匹配”，导致回显漏网。
func TestUpstreamErrorRedactsWhitespacePaddedKey(t *testing.T) {
	body := fmt.Sprintf(`{"error":{"code":"invalid_api_key","message":"bad key %s"}}`, testKey)
	cl, _, _ := llmTestClient(t, http.StatusUnauthorized, body)

	_, err := callLLMTest(t, cl, " \r\n"+testKey+"\n ")
	if err == nil {
		t.Fatal("上游 401 应返回错误")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("带空白 Key 的错误文本仍泄露密钥: %q", err)
	}
}

func TestUpstreamErrorRedactsKey(t *testing.T) {
	const key = "sk-secret-in-error"
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"message回显", http.StatusUnauthorized,
			fmt.Sprintf(`{"error":{"code":"invalid_api_key","message":"bad key %s"}}`, key)},
		{"code回显", http.StatusBadRequest,
			fmt.Sprintf(`{"error":{"code":"%s","message":"bad key"}}`, key)},
		{"HTTP200错误体回显", http.StatusOK,
			fmt.Sprintf(`{"error":{"code":"%s","message":"bad key %s"}}`, key, key)},
		{"非JSON错误体对照", http.StatusUnauthorized, "rejected " + key},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl, _, _ := llmTestClient(t, tc.status, tc.body)
			_, err := callLLMTest(t, cl, key)
			if err == nil {
				t.Fatal("应返回错误")
			}
			if strings.Contains(err.Error(), key) {
				t.Fatalf("错误文本泄露密钥: %q", err)
			}
			if tc.name != "非JSON错误体对照" && !strings.Contains(err.Error(), "（已隐藏）") {
				t.Fatalf("回显分支应带脱敏占位符: %q", err)
			}
		})
	}
}
