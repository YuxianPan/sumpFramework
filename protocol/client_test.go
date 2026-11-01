package protocol

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 连接没了以后，写失败必须归一到 ErrConnClosed。
//
// 回归闸：对端先一步消失时，WriteMessage 抛的是 TCP 层报错（connection reset by
// peer / broken pipe）。调用方以前拿到这个裸错误，errors.Is(..., ErrConnClosed)
// 判不出来——service 的 TestMaxMessageBytesInboundReport 就因此在慢机器上翻红。

// TestSendOnClosedConnIsErrConnClosed 是最确定的一条：连接已经关了，再发就是 ErrConnClosed。
func TestSendOnClosedConnIsErrConnClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, _ = ws.ReadMessage() // 挂着等客户端主动关
	}))
	defer server.Close()

	client, err := Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close() // 底层 conn 已关，后续写必失败

	req, _ := NewEnvelope(TypeJump, "caller", "peer", "", JumpPayload{Action: "echo"})
	if err := client.Send(req); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("closed conn send: got %v, want ErrConnClosed", err)
	}
}

// TestCallWhenPeerAborts 模拟真实场景：对端不等消息就把 TCP 掐了（不发 close frame）。
// 不管失败落在写上（Send 报错）还是读上（读循环关连接），调用方看到的都该是同一个错误。
func TestCallWhenPeerAborts(t *testing.T) {
	accepted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		close(accepted)
		_ = ws.UnderlyingConn().Close() // 裸 TCP 关闭：不给 close frame，等价于对端消失
	}))
	defer server.Close()

	client, err := Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	<-accepted
	time.Sleep(50 * time.Millisecond) // 等 FIN/RST 到达客户端

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := NewEnvelope(TypeJump, "caller", "peer", "", JumpPayload{Action: "echo"})
	if _, err := client.Call(ctx, req); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("call after peer abort: got %v, want ErrConnClosed", err)
	}
}
