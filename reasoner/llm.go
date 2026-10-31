// 千问（DashScope 的 OpenAI 兼容模式）客户端。
//
// 这里刻意做成无状态：钥匙、模型、上限都由每次调用带进来，不在结构体里缓存。
// 好处是设置一改下一次调用就生效，也不需要为「换了钥匙要重建连接」写任何同步代码。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultBaseURL 是 DashScope 的 OpenAI 兼容地址（API Key 与地域必须一致）。
	defaultBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	defaultModel   = "qwen-plus"

	// defaultMaxTokens 是默认上限；maxTokensLimit 是天花板——来自协议
	//（ThinkPayload.MaxTokens 注释），防止误填一个天文数字把账单打飞。
	defaultMaxTokens = 1024
	maxTokensLimit   = 4096

	// httpTimeout 是等上游的最长时间。骨架的动作超时在装配处按它加余量，
	// 保证先拿到上游的明确错误，而不是被骨架先砍断。
	httpTimeout = 90 * time.Second

	// maxBodyBytes 限制上游报文：正常回复只有几十 KB，限长只为挡住异常响应。
	maxBodyBytes = 1 << 20
)

// chatClient 可以被并发复用：http.Client 自带连接池，结构体本身没有可变状态。
type chatClient struct {
	hc *http.Client
}

func newChatClient() *chatClient {
	return &chatClient{hc: &http.Client{Timeout: httpTimeout}}
}

// chatRequest / chatResponse 只声明用得到的字段：多出来的字段一律忽略，
// 上游加字段不会把这里打挂（与协议「未知字段必须忽略」同一条规矩）。
type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	// Stream 恒为 false，但**故意不加 omitempty**：要把 "stream":false 显式发出去，
	// 免得对端按默认值理解。看着像没人用的字段，别删。
	Stream bool `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// think 发起一次补全：system 为空就不带 system 消息；maxTokens 由调用方夹好。
// finish 是上游给的停止原因（"length" = 被 max_tokens 截断），供调用方记日志。
func (c *chatClient) think(ctx context.Context, base, key, model, system, prompt string, maxTokens int) (string, string, error) {
	if strings.TrimSpace(key) == "" {
		return "", "", errors.New("还没有千问 API Key：面板设置 llm.key，或启动时给 -llm-key")
	}
	if strings.TrimSpace(model) == "" {
		model = defaultModel
	}

	msgs := make([]chatMessage, 0, 2)
	if strings.TrimSpace(system) != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: system})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: prompt})

	body, err := json.Marshal(chatRequest{Model: model, Messages: msgs, MaxTokens: maxTokens})
	if err != nil {
		return "", "", fmt.Errorf("编码请求失败: %w", err)
	}

	url := strings.TrimSuffix(strings.TrimSpace(base), "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("接口地址不可用（%s）: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", fmt.Errorf("请求被中断: %w", ctx.Err())
		}
		return "", "", fmt.Errorf("连不上千问（%s）: %w", url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", "", fmt.Errorf("读上游响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", upstreamError(resp.StatusCode, raw)
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", fmt.Errorf("上游响应不是合法 JSON（HTTP %d）: %w", resp.StatusCode, err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", "", fmt.Errorf("千问返回错误（%s）: %s", out.Error.Code, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", "", errors.New("千问没有返回任何结果（choices 为空）")
	}
	text := strings.TrimSpace(out.Choices[0].Message.Content)
	if text == "" {
		// 思考模型的正文在 content、思考过程在 reasoning_content：若思考把额度耗尽，
		// content 就是空的。所以这句要把「模型在思考」也列进来。
		return "", "", errors.New("千问返回空文本（可能被安全策略拦截、触发了长度上限，或模型开启了思考模式）")
	}
	return text, out.Choices[0].FinishReason, nil
}

// upstreamError 把非 200 翻成人话。只带上游自己的 code/message，绝不回显钥匙。
func upstreamError(status int, raw []byte) error {
	var out chatResponse
	_ = json.Unmarshal(raw, &out) // 尽力而为：错误体不保证是 JSON

	detail := ""
	if out.Error != nil {
		switch {
		case out.Error.Code != "" && out.Error.Message != "":
			detail = out.Error.Code + " / " + out.Error.Message
		case out.Error.Code != "":
			detail = out.Error.Code
		default:
			detail = out.Error.Message
		}
	}

	hint := ""
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		hint = "API Key 无效，或与所选地域不匹配"
	case status == http.StatusTooManyRequests:
		hint = "调用太频繁，或额度/余额不足"
	case status == http.StatusBadRequest:
		// 400 只回一句英文，翻成能照着做的动作。最常见的三样：
		// 默认开启思考的模型走了非流式、输入超了该模型的上下文、模型名或参数不认识。
		switch {
		case strings.Contains(detail, "enable_thinking"):
			hint = "该模型默认开启思考，非流式调用不被支持；请改用 qwen-plus / qwen-flash"
		case strings.Contains(detail, "input length"):
			hint = "提示词超过该模型的输入上限；请缩短 prompt/context，或换上下文更大的模型"
		default:
			hint = "参数不被接受（模型名或参数可能不支持）"
		}
	case status >= 500:
		hint = "上游服务异常，可稍后重试"
	}

	// 一句：HTTP 码 + 人话提示（有就带）+ 上游原文（有就带）。没有细节时不加冒号。
	msg := fmt.Sprintf("千问返回 HTTP %d", status)
	switch {
	case hint != "":
		msg += "：" + hint
		if detail != "" {
			msg += "（" + detail + "）"
		}
	case detail != "":
		msg += "：" + detail
	}
	return errors.New(msg)
}

// clampMaxTokens 把上限夹进 [1, 4096]：没填用默认，填大了按协议压到天花板。
func clampMaxTokens(n int) int {
	if n <= 0 {
		return defaultMaxTokens
	}
	if n > maxTokensLimit {
		return maxTokensLimit
	}
	return n
}
