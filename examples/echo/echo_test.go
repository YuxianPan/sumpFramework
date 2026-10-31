package main

// 示例的验收：起一个最小设置中心，跑起 echo，直接调用它的动作。
// 这也是「一个服务怎么被测试」的最小样板——服务自带 tests，用替身中心，不依赖外网。

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sumpFramework/protocol"
)

// stubCenter 是只做三件事的设置中心：应答注册、回一条空名册、收心跳。
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
			roster := protocol.RosterPayload{
				Services: []protocol.ServiceCard{{Name: "settings-center", Addr: sc.URL()}},
				Revision: 1,
			}
			payload, _ := protocol.EncodePayload(roster)
			resp, _ := protocol.NewResponse(env, "settings-center", protocol.ResponsePayload{OK: true, Data: payload})
			send(resp)
		}
	}
}

func (sc *stubCenter) registers() []protocol.RegisterPayload {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return append([]protocol.RegisterPayload(nil), sc.regs...)
}

// TestEchoService 覆盖示例服务的三条最小保证：
// 注册名片里有这个动作、动作能返回结果、设置项能改变行为。
func TestEchoService(t *testing.T) {
	center := startStubCenter(t)
	srv := newEcho("echo", "127.0.0.1:0", center.URL())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer srv.Shutdown()

	// 名片必须声明动作，否则调用方（和模型）看不见它。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if regs := center.registers(); len(regs) > 0 {
			if len(regs[0].Provides) != 1 || regs[0].Provides[0].Action != "echo" {
				t.Fatalf("名片未声明 echo 动作: %+v", regs[0])
			}
			if len(regs[0].Settings) != 1 || regs[0].Settings[0].Key != "prefix" {
				t.Fatalf("名片未声明 prefix 设置项: %+v", regs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("5 秒内没有收到注册")
		}
		time.Sleep(20 * time.Millisecond)
	}

	c, err := protocol.Dial(srv.WsURL(), nil)
	if err != nil {
		t.Fatalf("连接服务失败: %v", err)
	}
	defer c.Close()

	got := callEcho(t, ctx, c, "你好")
	if want := "echo: 你好"; got != want {
		t.Fatalf("返回 %q，期望 %q", got, want)
	}
}

func callEcho(t *testing.T, ctx context.Context, c *protocol.Client, text string) string {
	t.Helper()
	input, err := protocol.EncodePayload(echoInput{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "caller", "echo", "", protocol.JumpPayload{Action: "echo", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	env.Boss = "caller"

	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := c.Call(callCtx, env)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	var p protocol.ResponsePayload
	if err := resp.DecodePayload(&p); err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	if !p.OK {
		t.Fatalf("服务返回错误: %s", p.Error)
	}
	var out struct {
		Text string `msgpack:"text"`
	}
	if err := protocol.DecodeRaw(p.Data, &out); err != nil {
		t.Fatalf("解结果失败: %v", err)
	}
	return fmt.Sprintf("%s", out.Text)
}
