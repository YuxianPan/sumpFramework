package main

// 测试不联网、不花钱：上游是 httptest 假千问，中心是进程内替身。
// 按 docs/add-a-service.md 的口径，三类断言都要有：名片声明、正常路径、错误路径。

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sumpFramework/protocol"
	"github.com/NouSart/sumpFramework/service"
)

// testKey 是测试用的假钥匙；同时用来证明它不会出现在任何错误文本里。
const testKey = "sk-test-key"

// stubCenter 是只做必要应答的设置中心：注册（回一份含自己的名册）、拉设置、上报、心跳。
type stubCenter struct {
	ln   net.Listener
	srv  *http.Server
	mu   sync.Mutex
	regs []protocol.RegisterPayload
}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", sc.handleWS)
	sc.srv = &http.Server{Handler: mux}
	go func() { _ = sc.srv.Serve(ln) }()
	t.Cleanup(func() { _ = sc.srv.Close() })
	return sc
}

func (sc *stubCenter) URL() string { return "ws://" + sc.ln.Addr().String() + "/ws" }

func (sc *stubCenter) roster() protocol.RosterPayload {
	// 名册里必须包含中心自己：骨架注册成功后会拿它回头拉设置。
	return protocol.RosterPayload{
		Services: []protocol.ServiceCard{{Name: "settings-center", Addr: sc.URL()}},
		Revision: 1,
	}
}

func (sc *stubCenter) handleWS(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	var writeMu sync.Mutex
	send := func(env protocol.Envelope) {
		data, err := protocol.Marshal(env)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = ws.WriteMessage(websocket.BinaryMessage, data)
	}
	reply := func(req protocol.Envelope, data any, errMsg string) {
		raw, err := protocol.EncodePayload(data)
		if err != nil {
			return
		}
		env, err := protocol.NewResponse(req, "settings-center", protocol.ResponsePayload{
			OK:    errMsg == "",
			Data:  raw,
			Error: errMsg,
		})
		if err != nil {
			return
		}
		send(env)
	}

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeRegister:
			var reg protocol.RegisterPayload
			if err := env.DecodePayload(&reg); err != nil {
				continue
			}
			sc.mu.Lock()
			sc.regs = append(sc.regs, reg)
			sc.mu.Unlock()
			reply(env, sc.roster(), "")
		case protocol.TypeJump:
			var jp protocol.JumpPayload
			_ = env.DecodePayload(&jp)
			switch jp.Action {
			case "roster":
				reply(env, sc.roster(), "")
			case "list_settings":
				// 不保管任何覆盖值：服务就用自己声明的预设值。
				reply(env, protocol.SettingsResult{}, "")
			case "report":
				// 骨架是等响应的，必须回 ok。
				reply(env, nil, "")
			default:
				reply(env, nil, "unknown action: "+jp.Action)
			}
		}
		// 心跳是事件，不回应。
	}
}

func (sc *stubCenter) registers() []protocol.RegisterPayload {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return append([]protocol.RegisterPayload(nil), sc.regs...)
}

// fakeQwen 是假的千问：记下收到的请求，按预设的状态码与报文作答。
type fakeQwen struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []chatRequest
	auth []string
	code int
	body string
}

func startFakeQwen(t *testing.T) *fakeQwen {
	t.Helper()
	fq := &fakeQwen{
		code: http.StatusOK,
		// 故意带首尾空白：服务应当把它去掉再返回。
		body: `{"choices":[{"message":{"content":"  你好，我是千问。  "},"finish_reason":"stop"}]}`,
	}
	fq.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		fq.mu.Lock()
		fq.reqs = append(fq.reqs, req)
		fq.auth = append(fq.auth, r.Header.Get("Authorization"))
		code, body := fq.code, fq.body
		fq.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(fq.srv.Close)
	return fq
}

// answer 改掉应答内容（错误路径用）。
func (fq *fakeQwen) answer(code int, body string) {
	fq.mu.Lock()
	defer fq.mu.Unlock()
	fq.code, fq.body = code, body
}

// last 取最近一次请求与它的 Authorization 头。
func (fq *fakeQwen) last(t *testing.T) (chatRequest, string) {
	t.Helper()
	fq.mu.Lock()
	defer fq.mu.Unlock()
	if len(fq.reqs) == 0 {
		t.Fatal("千问没有收到任何请求")
	}
	return fq.reqs[len(fq.reqs)-1], fq.auth[len(fq.auth)-1]
}

func (fq *fakeQwen) count() int {
	fq.mu.Lock()
	defer fq.mu.Unlock()
	return len(fq.reqs)
}

// startReasoner 起一个真实服务（随机端口），并等它报到成功。
func startReasoner(t *testing.T, s *service.Service, center *stubCenter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		cancel()
		t.Fatalf("启动失败: %v", err)
	}
	t.Cleanup(func() {
		s.Shutdown()
		cancel()
	})

	deadline := time.Now().Add(5 * time.Second)
	for len(center.registers()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("5 秒内没有收到注册")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// callAction 发一次跳转并解出响应载荷——错误路径也要能看见 ok/error。
func callAction(t *testing.T, s *service.Service, action string, input any) protocol.ResponsePayload {
	t.Helper()
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "caller", "reasoner", "", protocol.JumpPayload{
		Action: action,
		Input:  raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	env.Boss = "caller"

	c, err := protocol.Dial(s.WsURL(), nil)
	if err != nil {
		t.Fatalf("连接服务失败: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	var p protocol.ResponsePayload
	if err := resp.DecodePayload(&p); err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	return p
}

// setup 起替身中心 + 假千问 + 真实服务，并写好 llm.base / llm.key。
// t.Chdir 让秘密文件落进临时目录：测试不碰仓库里的 data/。
func setup(t *testing.T, withKey bool) (*fakeQwen, *service.Service, *stubCenter) {
	t.Helper()
	t.Chdir(t.TempDir())

	center := startStubCenter(t)
	fq := startFakeQwen(t)

	s := newReasoner("reasoner", "127.0.0.1:0", center.URL())
	if err := s.ApplySetting("llm.base", fq.srv.URL); err != nil {
		t.Fatalf("设 llm.base 失败: %v", err)
	}
	if withKey {
		if err := s.ApplySetting("llm.key", testKey); err != nil {
			t.Fatalf("设 llm.key 失败: %v", err)
		}
	}
	startReasoner(t, s, center)
	return fq, s, center
}

// TestThinkDefaults 覆盖正常路径：名片、请求形状、返回值。
func TestThinkDefaults(t *testing.T) {
	fq, s, center := setup(t, true)

	// 名片：动作与三个设置项都要声明，否则调用方和面板看不见。
	reg := center.registers()[0]
	if len(reg.Provides) != 1 || reg.Provides[0].Action != "think" {
		t.Fatalf("名片未声明 think 动作: %+v", reg.Provides)
	}
	if reg.Provides[0].Tool {
		t.Fatal("think 不该标成工具动作（它是给别的服务用的能力）")
	}
	keys := map[string]bool{}
	for _, st := range reg.Settings {
		keys[st.Key] = true
	}
	for _, want := range []string{"llm.base", "llm.model", "llm.key"} {
		if !keys[want] {
			t.Fatalf("名片缺设置项 %q: %+v", want, reg.Settings)
		}
	}

	rp := callAction(t, s, "think", protocol.ThinkPayload{Prompt: "你好"})
	if !rp.OK {
		t.Fatalf("服务返回错误: %s", rp.Error)
	}
	var out protocol.ThinkResult
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatalf("解结果失败: %v", err)
	}
	if out.Text != "你好，我是千问。" {
		t.Fatalf("应返回去掉首尾空白的文本，实际 %q", out.Text)
	}

	req, auth := fq.last(t)
	if auth != "Bearer "+testKey {
		t.Fatalf("Authorization 应为 Bearer <key>，实际 %q", auth)
	}
	if req.Model != defaultModel {
		t.Fatalf("未带 model 时应回落到设置项，实际 %q", req.Model)
	}
	if req.MaxTokens != defaultMaxTokens {
		t.Fatalf("未带 max_tokens 时应为 %d，实际 %d", defaultMaxTokens, req.MaxTokens)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content != "你好" {
		t.Fatalf("没有 system 时只该有一条 user 消息，实际 %+v", req.Messages)
	}
	if req.Stream {
		t.Fatal("应为同步补全（stream=false）")
	}
}

// TestThinkOverridesAndClamps 覆盖「请求覆盖设置项」与 max_tokens 夹取。
func TestThinkOverridesAndClamps(t *testing.T) {
	fq, s, _ := setup(t, true)

	rp := callAction(t, s, "think", protocol.ThinkPayload{
		System:    "只回答一句话。",
		Prompt:    "解释 WebSocket",
		Model:     "qwen-flash",
		MaxTokens: 999999,
	})
	if !rp.OK {
		t.Fatalf("服务返回错误: %s", rp.Error)
	}
	req, _ := fq.last(t)
	if req.Model != "qwen-flash" {
		t.Fatalf("请求里的 model 应优先，实际 %q", req.Model)
	}
	if req.MaxTokens != maxTokensLimit {
		t.Fatalf("max_tokens 应被夹到 %d，实际 %d", maxTokensLimit, req.MaxTokens)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != "只回答一句话。" {
		t.Fatalf("有 system 时应先带 system 消息，实际 %+v", req.Messages)
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "解释 WebSocket" {
		t.Fatalf("user 消息不对: %+v", req.Messages[1])
	}
}

// TestThinkRejectsEmptyPrompt 覆盖输入校验：不该打上游。
func TestThinkRejectsEmptyPrompt(t *testing.T) {
	fq, s, _ := setup(t, true)

	rp := callAction(t, s, "think", protocol.ThinkPayload{Prompt: "   "})
	if rp.OK {
		t.Fatal("空 prompt 应被拒绝")
	}
	if !strings.Contains(rp.Error, "prompt") {
		t.Fatalf("错误里应说明是 prompt 的问题，实际 %q", rp.Error)
	}
	if n := fq.count(); n != 0 {
		t.Fatalf("空 prompt 不该打到上游，实际 %d 次", n)
	}
}

// TestThinkRejectsOverlongPrompt 覆盖问题过长：超过直接拒，不截断、不烧钱。
func TestThinkRejectsOverlongPrompt(t *testing.T) {
	fq, s, _ := setup(t, true)

	// 只超一个字：边界卡在“超过”，不是“等于”。
	rp := callAction(t, s, "think", protocol.ThinkPayload{Prompt: strings.Repeat("字", maxPromptRunes+1)})
	if rp.OK {
		t.Fatal("超长 prompt 应被拒绝")
	}
	if !strings.Contains(rp.Error, "太长") {
		t.Fatalf("错误里应说明太长，实际 %q", rp.Error)
	}
	if n := fq.count(); n != 0 {
		t.Fatalf("超长 prompt 不该打到上游，实际 %d 次", n)
	}

	// 正好等于上限：应当放行。
	if rp := callAction(t, s, "think", protocol.ThinkPayload{Prompt: strings.Repeat("字", maxPromptRunes)}); !rp.OK {
		t.Fatalf("等于上限不该被拒: %s", rp.Error)
	}
}

// TestThinkUpstreamErrorHidesKey 覆盖上游错误：要可读，且绝不回显密钥。
func TestThinkUpstreamErrorHidesKey(t *testing.T) {
	fq, s, _ := setup(t, true)
	fq.answer(http.StatusUnauthorized, `{"error":{"code":"invalid_api_key","message":"Incorrect API key provided."}}`)

	rp := callAction(t, s, "think", protocol.ThinkPayload{Prompt: "你好"})
	if rp.OK {
		t.Fatal("上游 401 应变成 ok:false")
	}
	for _, want := range []string{"401", "invalid_api_key", "API Key"} {
		if !strings.Contains(rp.Error, want) {
			t.Fatalf("错误里应包含 %q，实际 %q", want, rp.Error)
		}
	}
	if strings.Contains(rp.Error, testKey) {
		t.Fatalf("错误里不该出现密钥：%q", rp.Error)
	}
}

// TestThinkWithoutKey 覆盖缺钥匙：明确报错，不打上游。
func TestThinkWithoutKey(t *testing.T) {
	fq, s, _ := setup(t, false)

	rp := callAction(t, s, "think", protocol.ThinkPayload{Prompt: "你好"})
	if rp.OK {
		t.Fatal("没有 Key 时应 ok:false")
	}
	if !strings.Contains(rp.Error, "API Key") {
		t.Fatalf("错误应指向缺 Key，实际 %q", rp.Error)
	}
	if n := fq.count(); n != 0 {
		t.Fatalf("没有 Key 不该打到上游，实际 %d 次", n)
	}
}

// TestUnknownAction 确认骨架对未声明动作的兜底没被破坏。
func TestUnknownAction(t *testing.T) {
	_, s, _ := setup(t, true)

	rp := callAction(t, s, "no_such_action", nil)
	if rp.OK {
		t.Fatal("未知动作必须回 ok:false")
	}
	if !strings.Contains(rp.Error, "unknown action") {
		t.Fatalf("错误应指出未知动作，实际 %q", rp.Error)
	}
}

// TestClampMaxTokens 锁定夹取口径。
func TestClampMaxTokens(t *testing.T) {
	cases := map[int]int{
		0:      defaultMaxTokens,
		-5:     defaultMaxTokens,
		1:      1,
		4096:   maxTokensLimit,
		4097:   maxTokensLimit,
		999999: maxTokensLimit,
	}
	for in, want := range cases {
		if got := clampMaxTokens(in); got != want {
			t.Fatalf("clampMaxTokens(%d) = %d，期望 %d", in, got, want)
		}
	}
}

// TestValidateBaseURL 锁定地址校验：只认 http/https 且必须有主机名。
func TestValidateBaseURL(t *testing.T) {
	for _, v := range []string{"https://dashscope.aliyuncs.com/compatible-mode/v1", "http://127.0.0.1:8080/v1"} {
		if err := validateBaseURL(v); err != nil {
			t.Fatalf("%q 应被接受: %v", v, err)
		}
	}
	for _, v := range []string{"", "dashscope.aliyuncs.com", "ftp://x", "https://"} {
		if err := validateBaseURL(v); err == nil {
			t.Fatalf("%q 应被拒绝", v)
		}
	}
}

// TestUpstreamErrorHints 锁定报错翻译：上游只回一句英文，要翻成能照着做的动作。
// 400 的三条分支尤其重要——它们是「模型选错」时唯一能看到的线索。
func TestUpstreamErrorHints(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "思考模型走了非流式",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"InvalidParameter","message":"parameter.enable_thinking must be set to false for non-streaming calls"}}`,
			want:   "非流式",
		},
		{
			name:   "输入超模型上限",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"InvalidParameter","message":"Range of input length should be [1, 30720]"}}`,
			want:   "输入上限",
		},
		{
			name:   "模型名不认识",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"InvalidParameter","message":"Model not exist."}}`,
			want:   "参数不被接受",
		},
		{
			name:   "Key 不对",
			status: http.StatusUnauthorized,
			body:   `{"error":{"code":"invalid_api_key","message":"Incorrect API key provided."}}`,
			want:   "API Key",
		},
		{
			// 错误体不是 JSON 时也不该崩：只丢细节，HTTP 码和提示照给。
			name:   "错误体不是 JSON",
			status: http.StatusInternalServerError,
			body:   `oops`,
			want:   "上游服务异常",
		},
	}
	for _, c := range cases {
		err := upstreamError(c.status, []byte(c.body))
		if err == nil {
			t.Fatalf("%s: 应返回错误", c.name)
		}
		msg := err.Error()
		if !strings.Contains(msg, c.want) {
			t.Fatalf("%s: 错误里应含 %q，实际 %q", c.name, c.want, msg)
		}
		if !strings.Contains(msg, strconv.Itoa(c.status)) {
			t.Fatalf("%s: 错误里应带上 HTTP 码 %d，实际 %q", c.name, c.status, msg)
		}
	}
}

// TestThinkWithContext 覆盖「带资料」：逐条编号拼在问题前，问题原样落在最后。
func TestThinkWithContext(t *testing.T) {
	fq, s, _ := setup(t, true)

	// 第三条故意是空白：不该占编号。
	refs := []string{"  资料甲：WebSocket 是全双工。  ", "资料乙：握手借用 HTTP。", "   "}
	rp := callAction(t, s, "think", protocol.ThinkPayload{Prompt: "解释 WebSocket", Context: refs})
	if !rp.OK {
		t.Fatalf("服务返回错误: %s", rp.Error)
	}

	req, _ := fq.last(t)
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("带资料时仍该只有一条 user 消息（资料不单独成条），实际 %+v", req.Messages)
	}
	body := req.Messages[0].Content
	for _, want := range []string{"[1] 资料甲：WebSocket 是全双工。", "[2] 资料乙：握手借用 HTTP。"} {
		if !strings.Contains(body, want) {
			t.Fatalf("user 消息缺 %q:\n%s", want, body)
		}
	}
	if !strings.HasSuffix(body, "问题：解释 WebSocket") {
		t.Fatalf("问题应原样落在最后:\n%s", body)
	}
	if strings.Contains(body, "[3]") {
		t.Fatalf("空白条目不该占编号:\n%s", body)
	}
}

// TestThinkWithoutContextUnchanged 是回归闸：不填资料时，消息必须和以前一字不差。
func TestThinkWithoutContextUnchanged(t *testing.T) {
	fq, s, _ := setup(t, true)

	rp := callAction(t, s, "think", protocol.ThinkPayload{Prompt: "你好"})
	if !rp.OK {
		t.Fatalf("服务返回错误: %s", rp.Error)
	}
	req, _ := fq.last(t)
	if len(req.Messages) != 1 || req.Messages[0].Content != "你好" {
		t.Fatalf("不填资料时不该改动消息，实际 %+v", req.Messages)
	}
}

// TestBuildPrompt 覆盖拼装规则：原样透传、跳过空白、超长整条丢弃。
func TestBuildPrompt(t *testing.T) {
	if got, used, dropped := buildPrompt("问", nil); got != "问" || used != 0 || dropped != 0 {
		t.Fatalf("无资料应原样返回，实际 %q used=%d dropped=%d", got, used, dropped)
	}
	if got, used, dropped := buildPrompt("问", []string{"  ", ""}); got != "问" || used != 0 || dropped != 0 {
		t.Fatalf("全空白应退回裸问题，实际 %q used=%d dropped=%d", got, used, dropped)
	}

	// 超一条就超总额度：整条丢弃，一个字都不进，也不给它编号；后面的短条目照常进。
	long := strings.Repeat("长", maxContextRunes+1)
	got, used, dropped := buildPrompt("问", []string{long, "短的"})
	if used != 1 || dropped != 1 {
		t.Fatalf("超长条目应整条丢弃，实际 used=%d dropped=%d", used, dropped)
	}
	if strings.Contains(got, "长") {
		t.Fatal("超长条目不该部分进入（也不该占编号）")
	}
	if !strings.Contains(got, "[1] 短的") {
		t.Fatalf("后面的短条目应拿到 [1]:\n%s", got)
	}
}
