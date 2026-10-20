package media

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestASR_送16k单声道WAV取回文字(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/asr" {
			http.NotFound(w, r)
			return
		}
		got, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"text":" 明天十点开会 ","segments":1}`))
	}))
	defer srv.Close()

	// silk 不用 ffmpeg 也能解：QQ、微信的语音消息就是它。
	enc, _ := encodeSilk(tone(silkRate, 500), silkRate)
	a := &ASR{URL: srv.URL + "/", Codec: &Codec{FFmpeg: "no-such-ffmpeg-binary"}}
	text, err := a.Transcribe(context.Background(), enc)
	if err != nil || text != "明天十点开会" {
		t.Fatalf("应取回去掉空白的文字: %q %v", text, err)
	}
	if Sniff(got) != MIMEWAV || binary.LittleEndian.Uint32(got[24:28]) != 16000 || binary.LittleEndian.Uint16(got[22:24]) != 1 {
		t.Fatalf("送去的应是 16k 单声道 WAV: %q", got[:min(len(got), 44)])
	}
}

func TestASR_服务出错时带上原因(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"引擎未就绪（loading）"}`))
	}))
	defer srv.Close()
	enc, _ := encodeSilk(tone(silkRate, 200), silkRate)
	_, err := (&ASR{URL: srv.URL}).Transcribe(context.Background(), enc)
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "未就绪") {
		t.Fatalf("应报出服务给的原因: %v", err)
	}
}

// 接真的识别服务试一次：设了 ATOM_TEST_ASR（服务地址）和 ATOM_TEST_ASR_WAV（一段语音）才跑。
func TestASR_真服务(t *testing.T) {
	url, wav := os.Getenv("ATOM_TEST_ASR"), os.Getenv("ATOM_TEST_ASR_WAV")
	if url == "" || wav == "" {
		t.Skip("没有设 ATOM_TEST_ASR、ATOM_TEST_ASR_WAV，跳过")
	}
	data, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	// 先编成 silk，走一遍 QQ、微信语音消息的路子。
	pcm, err := Default.PCM(context.Background(), data, silkRate)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encodeSilk(pcm, silkRate)
	if err != nil {
		t.Fatal(err)
	}
	text, err := (&ASR{URL: url}).Transcribe(context.Background(), enc)
	if err != nil || text == "" {
		t.Fatalf("应转出文字: %q %v", text, err)
	}
	t.Logf("转出: %s", text)
}
