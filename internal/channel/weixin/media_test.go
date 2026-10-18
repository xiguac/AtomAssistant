package weixin

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/xiguac/atomhelper/internal/channel"
	"github.com/xiguac/atomhelper/internal/media"
)

func TestConvert_语音和视频按文件的办法取(t *testing.T) {
	b := &Bot{Label: "wx"}
	ref := `{"encrypt_query_param":"p","aes_key":"k"}`
	var m message
	if err := json.Unmarshal([]byte(`{"message_id":9,"from_user_id":"u1","message_type":1,"item_list":[`+
		`{"type":3,"voice_item":{"media":`+ref+`}},{"type":5,"video_item":{"media":`+ref+`}}]}`), &m); err != nil {
		t.Fatal(err)
	}
	out, ok := b.convert(context.Background(), &m)
	if !ok || len(out.Voices) != 1 || out.Voices[0].Fetch == nil || len(out.Videos) != 1 || out.Videos[0].Fetch == nil {
		t.Fatalf("语音、视频应带给上层: %+v", out)
	}
}

func TestSend_图片视频文件加密上传后引用发出(t *testing.T) {
	var mu sync.Mutex
	var asks []map[string]any
	var plains [][]byte
	var items []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/ilink/bot/getuploadurl":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			asks = append(asks, body)
			_, _ = io.WriteString(w, `{"ret":0,"upload_param":"up-`+strconv.Itoa(len(asks))+`"}`)
		case "/upload":
			// 用申请上传时报的密钥解开，核对内容、大小和摘要都对得上。
			ask := asks[len(asks)-1]
			if r.URL.Query().Get("filekey") != ask["filekey"] || r.URL.Query().Get("encrypted_query_param") != "up-"+strconv.Itoa(len(asks)) {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			key, _ := hex.DecodeString(ask["aeskey"].(string))
			cipher, _ := io.ReadAll(r.Body)
			plain, err := decryptECB(cipher, key)
			if err != nil || float64(len(cipher)) != ask["filesize"] || float64(len(plain)) != ask["rawsize"] {
				http.Error(w, "bad cipher", http.StatusBadRequest)
				return
			}
			sum := md5.Sum(plain)
			if hex.EncodeToString(sum[:]) != ask["rawfilemd5"] {
				http.Error(w, "bad md5", http.StatusBadRequest)
				return
			}
			plains = append(plains, plain)
			w.Header().Set("x-encrypted-param", "down-"+strconv.Itoa(len(plains)))
		case "/ilink/bot/sendmessage":
			var body struct {
				Msg struct {
					Items []map[string]any `json:"item_list"`
				} `json:"msg"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			items = append(items, body.Msg.Items...)
			_, _ = io.WriteString(w, `{"ret":0}`)
		}
	}))
	defer srv.Close()
	bot := &Bot{Label: "wx", BaseURL: srv.URL, CDNBase: srv.URL, State: &memKV{},
		Token: func(context.Context) (string, error) { return "bot-token-1", nil }}
	ctx := context.Background()
	mp4 := append([]byte{0, 0, 0, 0x18}, []byte("ftypisom-video-body")...)

	for _, f := range []channel.File{
		{Name: "图.png", Data: append([]byte{}, png...), Kind: media.KindImage},
		{Name: "片段.mp4", Data: mp4, Kind: media.KindVideo},
		{Name: "报价单.xlsx", Data: []byte("xlsx-body"), Kind: media.KindFile},
	} {
		f := f
		if err := bot.Send(ctx, channel.Outgoing{Chat: "u:user-1", File: &f}); err != nil {
			t.Fatalf("发 %s 失败: %v", f.Name, err)
		}
	}
	// 上传的类别：1 图片、2 视频、3 文件；对方是谁也报上去。
	for i, want := range []float64{1, 2, 3} {
		if asks[i]["media_type"] != want || asks[i]["to_user_id"] != "user-1" || asks[i]["no_need_thumb"] != true {
			t.Fatalf("第 %d 次申请上传不对: %v", i+1, asks[i])
		}
	}
	if string(plains[2]) != "xlsx-body" || len(items) != 3 {
		t.Fatalf("上传的内容或发出的消息不对: %d %d", len(plains), len(items))
	}
	// 消息里引用 CDN 给回的下载参数，密钥是那串十六进制字符的 Base64。
	img := items[0]["image_item"].(map[string]any)
	ref := img["media"].(map[string]any)
	keyHex, _ := base64.StdEncoding.DecodeString(ref["aes_key"].(string))
	if items[0]["type"] != float64(2) || ref["encrypt_query_param"] != "down-1" || string(keyHex) != asks[0]["aeskey"] ||
		ref["encrypt_type"] != float64(1) || img["mid_size"] != asks[0]["filesize"] {
		t.Fatalf("图片消息不对: %v", items[0])
	}
	if items[1]["type"] != float64(5) || items[1]["video_item"].(map[string]any)["video_size"] != asks[1]["filesize"] {
		t.Fatalf("视频消息不对: %v", items[1])
	}
	file := items[2]["file_item"].(map[string]any)
	if items[2]["type"] != float64(4) || file["file_name"] != "报价单.xlsx" || file["len"] != "9" {
		t.Fatalf("文件消息不对: %v", items[2])
	}
	// 语音：官方客户端没有发语音的做法，交给中枢改成按文件发。
	if err := bot.Send(ctx, channel.Outgoing{Chat: "u:user-1", File: &channel.File{Name: "录音.mp3", Data: []byte("ID3"), Kind: media.KindAudio}}); err != channel.ErrUnsupportedKind {
		t.Fatalf("语音应退回中枢: %v", err)
	}
}
