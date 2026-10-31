// reasoner 是 SUMP 的单推理器：收到一段提示词，交给千问，把文本还回去。
//
// 它只做一件事——单步补全。不执行动作（没有工具、没有多轮、没有循环），
// 也不记忆上下文（服务内不存任何对话；要上文，由调用方自己拼进 prompt）。
// 载荷就是协议里的 protocol.ThinkPayload / protocol.ThinkResult，不新增任何类型。
//
// 跑法：
//
//	go run ./reasoner -check -llm-key sk-xxx      自检：只打一次千问就退出（不启动服务、不落盘）
//	go run ./reasoner                             正常启动（key 已存在本机秘密文件里）
//	go run ./reasoner -llm-key sk-xxx             首次启动，顺手把 key 存下来（0600，git 忽略）
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/NouSart/sumpFramework/protocol"
	"github.com/NouSart/sumpFramework/service"
)

// defaultCheckPrompt 是自检的默认问题：flag 默认值与运行时的兜底共用这一处。
const defaultCheckPrompt = "用一句话介绍你自己。"

func main() {
	name := flag.String("name", "reasoner", "服务名（注册到设置中心的名字）")
	addr := flag.String("addr", "127.0.0.1:9102", "监听地址 host:port")
	center := flag.String("center", "ws://127.0.0.1:9000/ws", "设置中心地址")
	key := flag.String("llm-key", "", "千问 API Key（首次提供后会存进本机秘密文件，之后可省略）")
	base := flag.String("llm-base", "", "千问接口地址（留空用 DashScope 兼容模式默认值）")
	check := flag.Bool("check", false, "自检：打一次千问、打印回答后退出")
	checkPrompt := flag.String("check-prompt", defaultCheckPrompt, "自检时问的话")
	flag.Parse()

	// 自检：不启动服务、不注册、不写任何文件——只用本次参数打一次上游。
	if *check {
		cl := newChatClient()
		baseURL := *base
		if baseURL == "" {
			baseURL = defaultBaseURL
		}
		if err := runCheck(cl, *key, baseURL, *checkPrompt); err != nil {
			log.Fatalf("自检失败: %v", err)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	s := newReasoner(*name, *addr, *center)

	// 启动参数先落一次值：秘密进本机文件，普通项作为初始值
	//（之后中心下发的覆盖值仍然优先，中心才是设置的权威）。
	if *key != "" {
		if err := s.ApplySetting("llm.key", *key); err != nil {
			log.Fatalf("保存千问 Key 失败: %v", err)
		}
	}
	if *base != "" {
		if err := s.ApplySetting("llm.base", *base); err != nil {
			log.Fatalf("-llm-base 被拒绝: %v", err)
		}
	}

	if err := s.Start(ctx); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	defer s.Shutdown()

	log.Printf("reasoner 已就绪 %s", s.WsURL())
	<-ctx.Done()
}

// newReasoner 组装服务。main 与测试共用同一份装配，避免两边各写一遍。
func newReasoner(name, addr, center string) *service.Service {
	s := service.New(service.Config{
		Name:        name,
		Title:       "推理",
		Listen:      addr,
		Center:      center,
		Description: "单步推理：给一段提示词，回一段文本（不执行动作、不记忆上下文）",
		Provides: []protocol.Provide{
			// Tool 保持默认 false：这是给别的服务用的能力，不该直接暴露给上层 agent。
			{Action: "think", Input: "{system?, prompt, context?, model?, max_tokens?}", Output: "{text}"},
		},
		// 上游超时再加余量：先拿到上游的明确错误，而不是被骨架先砍断。
		ActionTimeout: httpTimeout + 10*time.Second,
	})

	// 普通项：值由设置中心保管，校验归服务。回调留空＝不缓存，每次调用现读。
	s.HandleSetting("llm.base", defaultBaseURL, validateBaseURL,
		service.Label("千问接口地址"),
		service.Hint("DashScope 的 OpenAI 兼容地址，末尾不用写 /chat/completions"))
	s.HandleSetting("llm.model", defaultModel, nil,
		service.Label("默认模型"),
		// 只放「默认不思考」的模型：本服务固定非流式，默认开启思考的模型会被上游拒。
		// qwen-plus / qwen-flash 都是 1M 上下文、最大输出 32768，装得下我们的长度上限。
		service.As("enum", "qwen-plus", "qwen-flash"),
		service.Hint("请求里带了 model 时以请求为准"))

	// 秘密项：值不进设置中心，由本服务自己持有（data/<name>-secret.json，0600，git 忽略）。
	s.HandleSecret("llm.key", nil,
		service.Label("千问 API Key"),
		service.Hint("只在面板提交那一次经过中心；值永不上线"))

	cl := newChatClient()
	s.Handle("think", func(ctx context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
		var p protocol.ThinkPayload
		if err := protocol.DecodeRaw(in.Input, &p); err != nil {
			return nil, fmt.Errorf("输入不是 ThinkPayload: %w", err)
		}
		if strings.TrimSpace(p.Prompt) == "" {
			return nil, errors.New("prompt 不能为空")
		}
		// 问题和资料都要有顶：超长问题不截断、直接拒，免得悄悄答错或烧钱。
		if n := len([]rune(p.Prompt)); n > maxPromptRunes {
			return nil, fmt.Errorf("prompt 太长（%d 字，上限 %d）", n, maxPromptRunes)
		}

		// 三项每次现读：设置改了下一次调用就生效，服务内没有缓存要同步。
		base := s.SettingValue("llm.base")
		key := s.SettingValue("llm.key")
		model := strings.TrimSpace(p.Model)
		if model == "" {
			model = s.SettingValue("llm.model")
		}
		maxTokens := clampMaxTokens(p.MaxTokens)

		// 参考资料由调用方查好、逐条送来；编号与截断归本服务，谁调都是同一种拼法。
		prompt, used, dropped := buildPrompt(p.Prompt, p.Context)
		if dropped > 0 {
			log.Printf("参考资料超长被丢弃 %d 条（采用 %d 条，上限 %d 字）", dropped, used, maxContextRunes)
		}

		start := time.Now()
		text, finish, err := cl.think(ctx, base, key, model, p.System, prompt, maxTokens)
		if err != nil {
			return nil, err
		}
		// 只记「做了什么」：不记问题、不记答案、不记钥匙。
		if finish == "length" {
			log.Printf("think 被 max_tokens=%d 截断：model=%s 耗时=%s 字数=%d",
				maxTokens, model, time.Since(start).Round(time.Millisecond), len([]rune(text)))
		}
		return protocol.ThinkResult{Text: text}, nil
	})

	return s
}

// validateBaseURL 只做最低限度的校验：必须是 http/https 地址且带主机名。
// 语义归服务自己判断（中心只保管与转发）。
func validateBaseURL(v string) error {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("不是合法地址: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("必须是 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return errors.New("缺少主机名")
	}
	return nil
}

// maxContextRunes 是参考资料的总字数上限：按条累加，装不下的整条丢弃（不切半条）。
const maxContextRunes = 6000

// maxPromptRunes 是问题本身的长度上限。超了直接报错、不悄悄截断——
// 问题被砍掉一截，答出来的东西就是错的，不如让调用方知道。
const maxPromptRunes = 32000

// buildPrompt 把参考资料逐条编号拼在问题前面。
// 返回拼好的 prompt、采用了几条、丢掉了几条（日志只记数字，不记内容）。
// refs 为空时原样返回 prompt：不填资料的老调用，行为一字不差。
func buildPrompt(prompt string, refs []string) (string, int, int) {
	if len(refs) == 0 {
		return prompt, 0, 0
	}
	var b strings.Builder
	b.WriteString("以下是我查到的参考资料，可能不全，也可能有无关内容：\n")
	used, dropped, total := 0, 0, 0
	for _, r := range refs {
		r = strings.TrimSpace(r)
		if r == "" {
			continue // 空白条目不算数，也不占额度
		}
		n := len([]rune(r))
		if total+n > maxContextRunes {
			dropped++
			continue
		}
		total += n
		used++
		fmt.Fprintf(&b, "\n[%d] %s\n", used, r)
	}
	if used == 0 {
		return prompt, 0, dropped // 一条都没用上：不塞空壳，退回裸问题
	}
	b.WriteString("\n请结合以上资料回答；资料不足以回答时直接说不足，不要编。\n\n问题：")
	b.WriteString(prompt)
	return b.String(), used, dropped
}

// runCheck 是自检：不启动服务、不注册、不写任何文件，只用本次参数打一次上游。
// 它故意不读本机秘密文件——自检要回答的就是「这条 key 此刻能不能用」。
func runCheck(cl *chatClient, key, base, prompt string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("自检需要 -llm-key（它不读本机秘密文件，也不落盘）")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = defaultCheckPrompt
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	start := time.Now()
	text, _, err := cl.think(ctx, base, key, defaultModel, "", prompt, defaultMaxTokens)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("问：" + prompt)
	fmt.Printf("答（%s，耗时 %s）：%s", defaultModel, time.Since(start).Round(time.Millisecond), text)
	fmt.Println()
	fmt.Println()
	return nil
}
