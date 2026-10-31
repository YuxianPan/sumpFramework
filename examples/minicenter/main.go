// minicenter 是只够本地跑通用的「假设置中心」。
//
// 它不保管设置：不存覆盖值、不做名册推送、不按心跳超时剔除任何服务。
// 它只把服务启动所必需的几件事答对，好让你在没有真中心时也能把链路跑起来：
//
//	register            → 回一份名册（名册里包含它自己：骨架会据此回头拉设置）
//	heartbeat           → 只读，不回应（它是事件）
//	jump: list_settings → 回「没有设置项」（服务注册后会自己拉一次）
//	jump: report        → 回 ok（骨架是等响应的）
//
// 两种跑法：
//
//	go run ./examples/minicenter                                        # 当中心，监听 9000
//	go run ./examples/minicenter -ask 你好 -to ws://127.0.0.1:9102/ws    # 当调用方，只发一跳
//
// 真中心一旦就绪，这个文件就该被删掉——它不是协议的一部分。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sumpFramework/protocol"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "当中心时的监听地址 host:port")
	ask := flag.String("ask", "", "当调用方：把这句话发给目标服务，打印回答后退出")
	to := flag.String("to", "ws://127.0.0.1:9102/ws", "当调用方时，目标服务的 WS 地址")
	svc := flag.String("svc", "reasoner", "当调用方时，目标服务名（信封的 to）")
	flag.Parse()

	if *ask != "" {
		if err := runAsk(*to, *svc, *ask); err != nil {
			log.Fatalf("调用失败: %v", err)
		}
		return
	}
	if err := runCenter(*addr); err != nil {
		log.Fatalf("中心退出: %v", err)
	}
}

// center 记住报到过的服务，用来拼名册。
type center struct {
	url   string
	mu    sync.Mutex
	cards []protocol.ServiceCard
}

func runCenter(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	c := &center{url: "ws://" + ln.Addr().String() + "/ws"}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", c.handleWS)
	srv := &http.Server{Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() { _ = srv.Serve(ln) }()

	log.Printf("假中心已就绪 %s（只应答注册，不保管设置）", c.url)
	<-ctx.Done()
	return srv.Close()
}

// roster 拼名册：自己排第一（骨架会按名字 settings-center 回头拉设置）。
func (c *center) roster() protocol.RosterPayload {
	c.mu.Lock()
	cards := make([]protocol.ServiceCard, 0, len(c.cards)+1)
	cards = append(cards, protocol.ServiceCard{Name: "settings-center", Addr: c.url})
	cards = append(cards, c.cards...)
	c.mu.Unlock()
	return protocol.RosterPayload{Services: cards, Revision: 1}
}

func (c *center) handleWS(w http.ResponseWriter, r *http.Request) {
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
			c.mu.Lock()
			c.cards = append(c.cards, reg.Card())
			c.mu.Unlock()
			log.Printf("已报到：%s（%s）", reg.Name, reg.Addr)
			reply(env, c.roster(), "")
		case protocol.TypeJump:
			var jp protocol.JumpPayload
			_ = env.DecodePayload(&jp)
			switch jp.Action {
			case "roster":
				reply(env, c.roster(), "")
			case "list_settings":
				reply(env, protocol.SettingsResult{}, "")
			case "report":
				reply(env, nil, "")
			default:
				reply(env, nil, "unknown action: "+jp.Action)
			}
		}
		// 心跳是事件，不回应。
	}
}

// runAsk 当一次调用方：连上目标服务、发一跳 think、打印回答。
func runAsk(addr, svc, prompt string) error {
	c, err := protocol.Dial(addr, nil)
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	defer c.Close()

	input, err := protocol.EncodePayload(protocol.ThinkPayload{Prompt: prompt})
	if err != nil {
		return err
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "minicenter", svc, "", protocol.JumpPayload{
		Action: "think",
		Input:  input,
	})
	if err != nil {
		return err
	}
	env.Boss = "minicenter"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	resp, err := c.Call(ctx, env)
	if err != nil {
		return fmt.Errorf("调用 %s 失败: %w", svc, err)
	}
	var p protocol.ResponsePayload
	if err := resp.DecodePayload(&p); err != nil {
		return fmt.Errorf("解响应失败: %w", err)
	}
	if !p.OK {
		return fmt.Errorf("服务返回错误: %s", p.Error)
	}
	var out protocol.ThinkResult
	if err := protocol.DecodeRaw(p.Data, &out); err != nil {
		return fmt.Errorf("解结果失败: %w", err)
	}

	fmt.Println()
	fmt.Println("问：" + prompt)
	fmt.Println("答：" + out.Text)
	fmt.Println()
	return nil
}
