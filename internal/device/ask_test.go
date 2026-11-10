package device

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeAsker struct {
	texts  []string
	images [][]AskImage
	err    error
}

func (f *fakeAsker) Ask(_ context.Context, text string, images []AskImage) error {
	f.texts = append(f.texts, text)
	f.images = append(f.images, images)
	return f.err
}

func TestAsk_校验并交给助手(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	if err := s.Ask(ctx, "你好", nil); !errors.Is(err, ErrUnavailable) {
		t.Errorf("没接渠道时应当是暂时不可用: %v", err)
	}
	a := &fakeAsker{}
	s.Asker = a
	if err := s.Ask(ctx, "  ", nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("空话应当被拒: %v", err)
	}
	if err := s.Ask(ctx, strings.Repeat("字", MaxAskText+1), nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("太长应当被拒: %v", err)
	}
	if err := s.Ask(ctx, "看这个", &AskImage{MIME: "image/gif", Data: []byte("x")}); !errors.Is(err, ErrInvalid) {
		t.Errorf("不支持的图片格式应当被拒: %v", err)
	}
	if err := s.Ask(ctx, "看这个", &AskImage{MIME: "image/png", Data: make([]byte, MaxAskImage+1)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("太大的截屏应当被拒: %v", err)
	}
	if err := s.Ask(ctx, " 明天有什么安排 ", &AskImage{MIME: "image/jpeg", Data: []byte("jpg")}); err != nil {
		t.Fatal(err)
	}
	if len(a.texts) != 1 || a.texts[0] != "明天有什么安排" || len(a.images[0]) != 1 {
		t.Errorf("交给助手的内容 = %v %v", a.texts, a.images)
	}
}

func TestHandler_ask(t *testing.T) {
	s, _, _ := setup(t)
	a := &fakeAsker{}
	s.Asker = a
	h := Handler(s, nil)
	_, tok := pairedDevice(t, s)
	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", Prefix+"ask", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	img := base64.StdEncoding.EncodeToString([]byte("pngbytes"))
	body, _ := json.Marshal(map[string]string{"text": "这是什么", "screenshot_base64": img, "screenshot_mime": "image/png"})
	if w := do(string(body)); w.Code != http.StatusAccepted {
		t.Fatalf("提问 = %d %s", w.Code, w.Body)
	}
	if len(a.texts) != 1 || string(a.images[0][0].Data) != "pngbytes" {
		t.Errorf("助手收到的 = %v %v", a.texts, a.images)
	}
	if w := do(`{"text":"x","screenshot_base64":"!!!"}`); w.Code != http.StatusBadRequest {
		t.Errorf("坏的 base64 = %d", w.Code)
	}
	a.err = errors.Join(ErrUnavailable)
	if w := do(`{"text":"你好"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("渠道没就绪应当 503，得到 %d", w.Code)
	}
	// 没带令牌。
	req := httptest.NewRequest("POST", Prefix+"ask", bytes.NewBufferString(`{"text":"hi"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("没令牌 = %d", w.Code)
	}
}
