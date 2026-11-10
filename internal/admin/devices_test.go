package admin

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/xiguac/atomassistant/internal/device"
)

// 添加设备之后的那一页要把配对码和二维码显示出来：配对码只此一次可见。
func TestDevicesPage_添加设备后显示配对码和二维码(t *testing.T) {
	hash, err := HashPassword("test-password-123")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{PasswordHash: hash, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	link := "atomassistant://pair?server=https%3A%2F%2Fatom.example.com&code=K7M29XQP"
	qr, err := qrSVG(link)
	if err != nil {
		t.Fatal(err)
	}
	d := devicesData{
		Base: "https://atom.example.com", Kinds: device.Kinds,
		New:  &device.Device{ID: "dev_1", Name: "我的 iPhone"},
		Code: "K7M2-9XQP", Link: link, QR: qr, Expires: time.Now().Add(10 * time.Minute),
	}
	var buf bytes.Buffer
	if err := s.tmpl["devices"].Execute(&buf, &view{CSRF: "x", Data: d}); err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"K7M2-9XQP", "<svg", "我的 iPhone", "https://atom.example.com", "data-plain"} {
		if !strings.Contains(out, want) {
			t.Errorf("页面里缺少 %q", want)
		}
	}
}
