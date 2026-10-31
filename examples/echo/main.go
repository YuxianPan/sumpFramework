// echo 是 SUMP 框架的最小服务示例：一个动作、一个设置项。
//
// 它演示了一个服务必须做的最小集合：
//   - 启动参数只有三样：自己的名字、监听地址、设置中心地址
//   - 名片里声明 provides（对外动作）与 settings（可设置项）
//   - 用 service.New 拿到注册 / 心跳 / 名册 / 重连 / 告别
//   - 只用 Handle 注册业务动作，不自己处理网络
//
// 跑法：
//
//	go run ./examples/echo -addr 127.0.0.1:9101 -center ws://127.0.0.1:9000/ws
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"

	"github.com/NouSart/sumpFramework/protocol"
	"github.com/NouSart/sumpFramework/service"
)

// echoInput 是 echo 动作的输入。字段名与协议一致：snake_case。
type echoInput struct {
	Text string `msgpack:"text"`
}

func main() {
	name := flag.String("name", "echo", "服务名（注册到设置中心的名字）")
	addr := flag.String("addr", "127.0.0.1:9101", "监听地址 host:port")
	center := flag.String("center", "ws://127.0.0.1:9000/ws", "设置中心地址")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	s := newEcho(*name, *addr, *center)
	if err := s.Start(ctx); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	defer s.Shutdown()
	log.Printf("echo 已就绪 %s", s.WsURL())
	<-ctx.Done()
}

// newEcho 组装服务。main 与测试共用同一份装配，避免示例和用例各写一遍。
func newEcho(name, addr, center string) *service.Service {
	var (
		mu     sync.Mutex
		prefix = "echo: "
	)

	s := service.New(service.Config{
		Name:        name,
		Title:       "回声",
		Listen:      addr,
		Center:      center,
		Description: "最小示例服务：把收到的文本加上前缀返回",
		Provides: []protocol.Provide{
			{Action: "echo", Input: "{text}", Output: "{text}"},
		},
	})

	// 设置项：值由设置中心保管，变更时回调到这里。留空即关闭前缀。
	s.HandleSetting("prefix", prefix, func(v string) error {
		mu.Lock()
		defer mu.Unlock()
		prefix = v
		return nil
	}, service.Label("回声前缀"), service.Hint("加在返回文本前面的字符串，留空表示不加"))

	s.Handle("echo", func(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
		var p echoInput
		if err := protocol.DecodeRaw(in.Input, &p); err != nil {
			return nil, err
		}
		mu.Lock()
		head := prefix
		mu.Unlock()
		return map[string]string{"text": head + p.Text}, nil
	})

	return s
}
