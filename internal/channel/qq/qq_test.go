package qq

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"encoding/base64"
	"github.com/xiguac/atomassistant/internal/channel"
	"github.com/xiguac/atomassistant/internal/media"
)

// fakeQQ 模拟 QQ 开放平台：凭证接口、网关地址接口、WebSocket 网关、发消息接口。
type fakeQQ struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	tokenCalls int
	gotSecret  string
	frames     []payload // 网关收到的帧
	posts      []map[string]any
	postPaths  []string
	rejectIDs  bool // 带 msg_id 的发送一律拒绝，模拟被动回复过期
	conns      int
	push       chan payload // 测试往网关连接里塞的帧
	drop       chan struct{}
}

func newFake(t *testing.T) *fakeQQ {
	f := &fakeQQ{t: t, push: make(chan payload, 8), drop: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/app/getAppAccessToken", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.tokenCalls++
		f.gotSecret = in["clientSecret"]
		f.mu.Unlock()
		if in["appId"] != "1024" || in["clientSecret"] != "s3cret" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":100016,"message":"invalid appid or secret"}`))
			return
		}
		// 真实接口把有效期写成字符串。
		_, _ = w.Write([]byte(`{"access_token":"tok-1","expires_in":"7200"}`))
	})
	mux.HandleFunc("/gateway/bot", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "QQBot tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"url": "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/websocket", "shards": 1})
	})
	up := websocket.Upgrader{}
	mux.HandleFunc("/websocket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		f.mu.Lock()
		f.conns++
		f.mu.Unlock()
		_ = conn.WriteJSON(payload{Op: opHello, Data: json.RawMessage(`{"heartbeat_interval":80}`)})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				var p payload
				if err := conn.ReadJSON(&p); err != nil {
					return
				}
				f.mu.Lock()
				f.frames = append(f.frames, p)
				f.mu.Unlock()
				switch p.Op {
				case opIdentify:
					_ = conn.WriteJSON(payload{Op: opDispatch, Seq: 1, Type: eventReady, Data: json.RawMessage(`{"session_id":"sess-1"}`)})
				case opHeartbeat:
					_ = conn.WriteJSON(payload{Op: opHeartbeatAck})
				}
			}
		}()
		for {
			select {
			case p := <-f.push:
				_ = conn.WriteJSON(p)
			case <-f.drop:
				return
			case <-done:
				return
			}
		}
	})
	mux.HandleFunc("/v2/users/", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.posts = append(f.posts, body)
		f.postPaths = append(f.postPaths, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		if _, has := body["msg_id"]; has && f.rejectIDs {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":40034024,"message":"msg_id expired"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/files") {
			_, _ = w.Write([]byte(`{"file_uuid":"u-1","file_info":"FILE-INFO-1","ttl":3600}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"sent-1"}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeQQ) bot() *Bot {
	return &Bot{
		Label: "qq", AppID: "1024",
		Secret:   func(context.Context) (string, error) { return "s3cret", nil },
		TokenURL: f.srv.URL + "/app/getAppAccessToken", APIBase: f.srv.URL,
	}
}

func (f *fakeQQ) frame(op int) (payload, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.frames) - 1; i >= 0; i-- {
		if f.frames[i].Op == op {
			return f.frames[i], true
		}
	}
	return payload{}, false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func c2c(seq uint32, id, openid, text string) payload {
	d, _ := json.Marshal(map[string]any{
		"id": id, "content": text, "timestamp": "2026-10-09T13:00:00+08:00",
		"author": map[string]string{"id": openid, "user_openid": openid},
	})
	return payload{Op: opDispatch, Seq: seq, Type: eventC2C, ID: "evt-" + id, Data: d}
}

func TestListen_鉴权收消息心跳与断线恢复(t *testing.T) {
	f := newFake(t)
	b := f.bot()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var got []channel.Message
	done := make(chan error, 1)
	go func() {
		done <- b.Listen(ctx, func(m channel.Message) {
			mu.Lock()
			got = append(got, m)
			mu.Unlock()
		})
	}()
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(got) }

	// 鉴权：带着换来的凭证，订阅单聊事件。
	waitFor(t, "鉴权", func() bool { _, ok := f.frame(opIdentify); return ok })
	id, _ := f.frame(opIdentify)
	var ident struct {
		Token   string `json:"token"`
		Intents int    `json:"intents"`
		Shard   []int  `json:"shard"`
	}
	if err := json.Unmarshal(id.Data, &ident); err != nil || ident.Token != "QQBot tok-1" || ident.Intents != 1<<25 || len(ident.Shard) != 2 {
		t.Fatalf("鉴权帧不对: %s", id.Data)
	}

	f.push <- c2c(2, "msg-1", "OPENID_A", " 明天几点开会 ")
	waitFor(t, "收到消息", func() bool { return count() == 1 })
	mu.Lock()
	m := got[0]
	mu.Unlock()
	if m.Chat != "c2c:OPENID_A" || m.Sender != "OPENID_A" || m.Text != " 明天几点开会 " || m.ID != "msg-1" || m.At.IsZero() {
		t.Fatalf("消息内容不对: %+v", m)
	}
	// 同一条消息再推一遍（断线恢复时会发生）：不重复处理。
	f.push <- c2c(3, "msg-1", "OPENID_A", "明天几点开会")
	// 标识里带着路径字符的消息直接丢掉，它会被拼进接口地址。
	f.push <- c2c(4, "msg-x", "../../admin", "x")
	f.push <- c2c(5, "msg-2", "OPENID_A", "第二条")
	waitFor(t, "第二条消息", func() bool { return count() == 2 })
	mu.Lock()
	if got[1].ID != "msg-2" {
		t.Fatalf("重复的和不合法的应被丢掉: %+v", got)
	}
	mu.Unlock()

	// 心跳带着最新的序号。
	waitFor(t, "心跳", func() bool {
		p, ok := f.frame(opHeartbeat)
		return ok && string(p.Data) == "5"
	})

	// 连接断了：重连时接着上一次的会话收，而不是从头来。
	f.drop <- struct{}{}
	waitFor(t, "恢复", func() bool { _, ok := f.frame(opResume); return ok })
	res, _ := f.frame(opResume)
	var resume struct {
		Token     string `json:"token"`
		SessionID string `json:"session_id"`
		Seq       uint32 `json:"seq"`
	}
	if err := json.Unmarshal(res.Data, &resume); err != nil || resume.SessionID != "sess-1" || resume.Seq != 5 || resume.Token != "QQBot tok-1" {
		t.Fatalf("恢复帧不对: %s", res.Data)
	}
	f.push <- c2c(6, "msg-3", "OPENID_A", "断线之后的")
	waitFor(t, "恢复后收到消息", func() bool { return count() == 3 })

	// 凭证在有效期内只取一次。
	f.mu.Lock()
	calls := f.tokenCalls
	f.mu.Unlock()
	if calls != 1 {
		t.Fatalf("凭证应缓存，实际取了 %d 次", calls)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("正常停止不应报错: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 结束后 Listen 应退出")
	}
}

func TestSend_被动回复与改为主动(t *testing.T) {
	f := newFake(t)
	b := f.bot()
	ctx := context.Background()

	if err := b.Send(ctx, channel.Outgoing{Chat: "c2c:OPENID_A", Text: "十点", ReplyTo: "msg-1", Seq: 2}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if f.postPaths[0] != "POST /v2/users/OPENID_A/messages QQBot tok-1" {
		t.Fatalf("接口路径或凭证不对: %s", f.postPaths[0])
	}
	p := f.posts[0]
	if p["content"] != "十点" || p["msg_type"] != float64(0) || p["msg_id"] != "msg-1" || p["msg_seq"] != float64(2) {
		t.Fatalf("请求体不对: %v", p)
	}

	// 任务办完时离用户发话已经很久，旧消息的编号过期了：改成主动消息再发。
	f.rejectIDs = true
	if err := b.Send(ctx, channel.Outgoing{Chat: "c2c:OPENID_A", Text: "办完了", ReplyTo: "msg-old", Seq: 1}); err != nil {
		t.Fatalf("应改为主动消息后发出: %v", err)
	}
	last := f.posts[len(f.posts)-1]
	if _, has := last["msg_id"]; has || last["content"] != "办完了" || len(f.posts) != 3 {
		t.Fatalf("重试时不应再带消息编号: %v（共 %d 次）", last, len(f.posts))
	}
	// 没有可回应的消息：直接主动发。
	if err := b.Send(ctx, channel.Outgoing{Chat: "c2c:OPENID_A", Text: "提醒"}); err != nil || len(f.posts) != 4 {
		t.Fatalf("主动消息发送失败: %v", err)
	}

	for name, m := range map[string]channel.Outgoing{
		"不是单聊会话":  {Chat: "group:G1", Text: "x"},
		"标识带路径字符": {Chat: "c2c:../../admin", Text: "x"},
		"空消息":     {Chat: "c2c:OPENID_A", Text: "  "},
	} {
		if err := b.Send(ctx, m); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: 期望 ErrInvalid，实际 %v", name, err)
		}
	}
	if len(f.posts) != 4 {
		t.Fatal("不合法的消息不应发出请求")
	}
}

func TestAccessToken_密钥不对时报错且不外泄(t *testing.T) {
	f := newFake(t)
	b := f.bot()
	b.Secret = func(context.Context) (string, error) { return "wrong-secret", nil }
	err := b.Send(context.Background(), channel.Outgoing{Chat: "c2c:OPENID_A", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "AppSecret") {
		t.Fatalf("凭证取不到应说明原因: %v", err)
	}
	if strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("错误信息里不应出现密钥: %v", err)
	}

	locked := f.bot()
	locked.Secret = func(context.Context) (string, error) { return "", errors.New("保险库锁着") }
	if err := locked.Send(context.Background(), channel.Outgoing{Chat: "c2c:OPENID_A", Text: "x"}); err == nil || !strings.Contains(err.Error(), "保险库锁着") {
		t.Fatalf("取不到密钥应如实报出: %v", err)
	}
}

func TestSend_文件先上传再作为富媒体消息发出(t *testing.T) {
	f := newFake(t)
	b := f.bot()
	ctx := context.Background()
	file := &channel.File{Name: "行程.docx", Data: []byte("PK-docx-bytes")}

	if err := b.Send(ctx, channel.Outgoing{Chat: "c2c:OPENID_A", ReplyTo: "msg-1", Seq: 3, File: file}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if len(f.posts) != 2 || f.postPaths[0] != "POST /v2/users/OPENID_A/files QQBot tok-1" ||
		f.postPaths[1] != "POST /v2/users/OPENID_A/messages QQBot tok-1" {
		t.Fatalf("应先上传再发消息: %v", f.postPaths)
	}
	up := f.posts[0]
	// 内容是 base64 传上去的；只上传，不让平台代发。
	if up["file_type"] != float64(4) || up["file_data"] != "UEstZG9jeC1ieXRlcw==" || up["file_name"] != "行程.docx" || up["srv_send_msg"] != false {
		t.Fatalf("上传请求不对: %v", up)
	}
	msg := f.posts[1]
	media, _ := msg["media"].(map[string]any)
	if msg["msg_type"] != float64(7) || media["file_info"] != "FILE-INFO-1" || msg["msg_id"] != "msg-1" || msg["msg_seq"] != float64(3) {
		t.Fatalf("消息请求不对: %v", msg)
	}

	// 空文件不发请求。
	before := len(f.posts)
	err := b.Send(ctx, channel.Outgoing{Chat: "c2c:OPENID_A", File: &channel.File{Name: "空.docx"}})
	if !errors.Is(err, ErrInvalid) || len(f.posts) != before {
		t.Fatalf("空文件应被拒绝且不发请求: %v", err)
	}
}

func TestDispatch_消息里的图片附件带给上层(t *testing.T) {
	b := &Bot{Label: "qq"}
	var got []channel.Message
	data := `{"id":"m1","content":"看看这个","timestamp":"2026-10-13T10:00:00+08:00","author":{"user_openid":"OPENID1"},` +
		`"attachments":[{"content_type":"image/jpeg","filename":"a.jpg","url":"https://multimedia.nt.qq.com.cn/a.jpg"},` +
		`{"content_type":"voice","url":"https://multimedia.nt.qq.com.cn/b.silk"},{"content_type":"image/png","url":""},` +
		`{"content_type":"file","filename":"合同.pdf","url":"https://multimedia.nt.qq.com.cn/c.pdf"},` +
		`{"content_type":"audio/mpeg","filename":"录音.mp3","url":"https://multimedia.nt.qq.com.cn/e.mp3"}]}`
	b.dispatch(payload{Op: opDispatch, Type: eventC2C, Data: json.RawMessage(data)}, &session{}, newRecent(8),
		func(m channel.Message) { got = append(got, m) })
	if len(got) != 1 || got[0].Text != "看看这个" {
		t.Fatalf("消息没有交给上层: %+v", got)
	}
	// 图片要有地址才收。
	if imgs := got[0].Images; len(imgs) != 1 || imgs[0].URL != "https://multimedia.nt.qq.com.cn/a.jpg" || imgs[0].ContentType != "image/jpeg" {
		t.Fatalf("图片附件不对: %+v", got[0].Images)
	}
	// 图片以外的附件（语音消息、视频除外）当作文件，带着文件名。当作文件发来的音频也是文件。
	if fs := got[0].Files; len(fs) != 2 || fs[0].Name != "合同.pdf" || fs[0].URL != "https://multimedia.nt.qq.com.cn/c.pdf" || fs[1].Name != "录音.mp3" {
		t.Fatalf("文件附件不对: %+v", got[0].Files)
	}
	// 语音单独带给上层，格式由上层取回之后按内容认。
	if vs := got[0].Voices; len(vs) != 1 || vs[0].URL != "https://multimedia.nt.qq.com.cn/b.silk" {
		t.Fatalf("语音附件不对: %+v", got[0].Voices)
	}
	got = nil
	video := `{"id":"m2","content":"","timestamp":"2026-10-13T10:00:00+08:00","author":{"user_openid":"OPENID1"},` +
		`"attachments":[{"content_type":"video/mp4","url":"https://multimedia.nt.qq.com.cn/d.mp4"}]}`
	b.dispatch(payload{Op: opDispatch, Type: eventC2C, Data: json.RawMessage(video)}, &session{}, newRecent(8),
		func(m channel.Message) { got = append(got, m) })
	if len(got) != 1 || len(got[0].Videos) != 1 || got[0].Videos[0].URL != "https://multimedia.nt.qq.com.cn/d.mp4" {
		t.Fatalf("视频附件不对: %+v", got)
	}
}

func TestSend_图片语音视频按各自的类型上传(t *testing.T) {
	f := newFake(t)
	b := f.bot()
	ctx := context.Background()
	wav := media.WAV(make([]byte, 24000*2), 24000) // 1 秒静音
	for _, c := range []struct {
		file     channel.File
		fileType float64
		name     string
	}{
		{channel.File{Name: "图.png", Data: []byte("PNG-bytes"), Kind: media.KindImage}, 1, "图.png"},
		{channel.File{Name: "片段.mp4", Data: []byte("mp4-bytes"), Kind: media.KindVideo}, 2, "片段.mp4"},
		// QQ 的语音只收 silk：先转好，名字也换成 .silk。
		{channel.File{Name: "录音.wav", Data: wav, Kind: media.KindAudio}, 3, "录音.silk"},
	} {
		before := len(f.posts)
		file := c.file
		if err := b.Send(ctx, channel.Outgoing{Chat: "c2c:OPENID_A", File: &file}); err != nil {
			t.Fatalf("%s 发送失败: %v", c.name, err)
		}
		up := f.posts[before]
		if up["file_type"] != c.fileType || up["file_name"] != c.name {
			t.Fatalf("%s 上传请求不对: %v %v", c.name, up["file_type"], up["file_name"])
		}
		if c.fileType == 3 {
			raw, _ := base64.StdEncoding.DecodeString(up["file_data"].(string))
			if media.Sniff(raw) != media.MIMESilk {
				t.Fatalf("语音应转成 silk 再传: %s", media.Sniff(raw))
			}
		}
	}
}
