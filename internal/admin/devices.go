package admin

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/device"
)

// 「设备」页：手机上的 App 是一个数据源。在这里添加设备、拿到配对码和二维码，
// 手机扫码（或手填地址与配对码）后配对成功；之后可以看各设备上报了什么、吊销设备、清掉数据。
//
// 设备接口本身（device.Handler）挂在后台同一个监听上，用设备令牌鉴权，不走登录会话。

// deviceBaseSetting 是 App 要连的服务器地址，二维码里带的就是它。
const deviceBaseSetting = "device.base_url"

type deviceRow struct {
	*device.Device
	// Status 是 waiting（等待配对）、online、revoked 之一。
	Status string
	Stats  []deviceStat
}

type deviceStat struct {
	Label  string
	Kind   string
	Count  int
	Latest time.Time
}

type devicesData struct {
	Devices []deviceRow
	Pending []*pendingRow
	// Base 是 App 要连的服务器地址（已保存的，或按当前访问的域名给出的建议）。
	Base string
	// 刚添加的设备的配对信息，只在添加后的那一次响应里出现。
	New     *device.Device
	Code    string
	Link    string
	QR      template.HTML
	Expires time.Time
	Kinds   []device.Kind
	Choices []int
	// ReplyChannel 是手机提问的答复发到哪个消息渠道；Channels 是已配置的渠道名，供选择。
	ReplyChannel string
	Channels     []string
}

type pendingRow struct {
	*device.Request
	DeviceName string
	KindLabel  string
}

func kindLabel(id string) string {
	if k, ok := device.KindByID(id); ok {
		return k.Label
	}
	return id
}

func (s *Server) deviceBase(r *http.Request) (string, error) {
	base, err := s.opts.Store.Setting(r.Context(), deviceBaseSetting)
	if err != nil {
		return "", err
	}
	if base == "" {
		scheme := "https"
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
			scheme = "http"
		}
		base = scheme + "://" + r.Host
	}
	return base, nil
}

func (s *Server) loadDevices(r *http.Request) (devicesData, error) {
	d := devicesData{Kinds: device.Kinds, Choices: device.SyncChoices}
	var err error
	if d.Base, err = s.deviceBase(r); err != nil {
		return d, err
	}
	if d.ReplyChannel, err = s.opts.Store.Setting(r.Context(), device.ReplyChannelSetting); err != nil {
		return d, err
	}
	if d.ReplyChannel == "" {
		d.ReplyChannel = device.DefaultReplyChannel
	}
	if _, list, err := s.readChannels(); err == nil {
		for _, c := range list {
			d.Channels = append(d.Channels, c.Name)
		}
	}
	devs, err := s.opts.Store.ListDevices(r.Context())
	if err != nil {
		return d, err
	}
	names := map[string]string{}
	now := time.Now()
	for _, dev := range devs {
		names[dev.ID] = dev.Name
		row := deviceRow{Device: dev, Status: "online"}
		switch {
		case !dev.RevokedAt.IsZero():
			row.Status = "revoked"
		case dev.PairedAt.IsZero():
			row.Status = "waiting"
			if !dev.PairExpiresAt.After(now) {
				row.Status = "expired"
			}
		}
		if dev.Paired() {
			stats, err := s.opts.Store.DataStats(r.Context(), dev.ID)
			if err != nil {
				return d, err
			}
			for _, st := range stats {
				row.Stats = append(row.Stats, deviceStat{Label: kindLabel(st.Kind), Kind: st.Kind, Count: st.Count, Latest: st.Latest})
			}
		}
		d.Devices = append(d.Devices, row)
	}
	pending, err := s.opts.Store.PendingRequests(r.Context(), "", now.UTC())
	if err != nil {
		return d, err
	}
	for _, p := range pending {
		d.Pending = append(d.Pending, &pendingRow{Request: p, DeviceName: names[p.DeviceID], KindLabel: kindLabel(p.Kind)})
	}
	return d, nil
}

func (s *Server) devicesPage(w http.ResponseWriter, r *http.Request, v *view) {
	d, err := s.loadDevices(r)
	if err != nil {
		v.Error = err.Error()
	}
	v.Data = d
	s.render(w, "devices", v)
}

// deviceAdd 添加一台设备，并在同一次响应里显示配对码和二维码——配对码不落库，刷新后就看不到了。
func (s *Server) deviceAdd(w http.ResponseWriter, r *http.Request, v *view) {
	if s.opts.Devices == nil {
		back(w, r, "/devices", "", errors.New("手机数据源未启用"))
		return
	}
	base, err := device.NormalizeBaseURL(r.PostFormValue("base"))
	var dev *device.Device
	var code string
	if err == nil {
		dev, code, err = s.opts.Devices.AddDevice(r.Context(), r.PostFormValue("name"))
	}
	if err == nil {
		err = s.opts.Store.PutSetting(r.Context(), deviceBaseSetting, base)
	}
	if err != nil {
		back(w, r, "/devices", "", err)
		return
	}
	d, loadErr := s.loadDevices(r)
	if loadErr != nil {
		v.Error = loadErr.Error()
	}
	d.New, d.Code, d.Expires = dev, device.FormatPairCode(code), dev.PairExpiresAt
	d.Link = "atomassistant://pair?server=" + url.QueryEscape(base) + "&code=" + code
	if d.QR, err = qrSVG(d.Link); err != nil {
		v.Error = err.Error()
	}
	s.opts.Log.Info("已添加手机设备，等待配对", "设备", dev.Name, "来自", r.RemoteAddr)
	v.Data = d
	s.render(w, "devices", v)
}

// deviceInterval 设置一台手机的上报间隔。App 下次同步时读到并照办。
func (s *Server) deviceInterval(w http.ResponseWriter, r *http.Request, _ *view) {
	id := strings.TrimSpace(r.PostFormValue("id"))
	minutes, err := strconv.Atoi(r.PostFormValue("minutes"))
	if err != nil {
		back(w, r, "/devices", "", errors.New("上报间隔要选一个数"))
		return
	}
	if s.opts.Devices == nil {
		back(w, r, "/devices", "", errors.New("手机数据源未启用"))
		return
	}
	err = s.opts.Devices.SetSyncMinutes(r.Context(), id, minutes)
	if err == nil {
		s.opts.Log.Info("手机的上报间隔已在后台修改", "设备", id, "分钟", minutes, "来自", r.RemoteAddr)
	}
	back(w, r, "/devices", "上报间隔已改为每 "+strconv.Itoa(minutes)+" 分钟，手机下次同步时生效。", err)
}

// deviceReplyChannel 设置手机提问的答复发到哪个消息渠道。
func (s *Server) deviceReplyChannel(w http.ResponseWriter, r *http.Request, _ *view) {
	name := strings.TrimSpace(r.PostFormValue("channel"))
	err := func() error {
		_, list, err := s.readChannels()
		if err != nil {
			return err
		}
		for _, c := range list {
			if c.Name == name {
				return s.opts.Store.PutSetting(r.Context(), device.ReplyChannelSetting, name)
			}
		}
		return errors.New("没有名为「" + name + "」的消息渠道，请先在「消息源」页配置")
	}()
	if err == nil {
		s.opts.Log.Info("手机提问的回复渠道已在后台修改", "渠道", name, "来自", r.RemoteAddr)
	}
	back(w, r, "/devices", "手机提问的答复会发到「"+name+"」。", err)
}

// deviceAdmin 打开或关闭一台手机的管理权限：打开后它的 App 能看任务、批准动作、回复和取消。
func (s *Server) deviceAdmin(w http.ResponseWriter, r *http.Request, _ *view) {
	id := strings.TrimSpace(r.PostFormValue("id"))
	on := r.PostFormValue("on") == "1"
	var err error
	if s.opts.Devices == nil {
		err = errors.New("手机数据源未启用")
	} else {
		err = s.opts.Devices.SetAdminAllowed(r.Context(), id, on)
	}
	msg := "这台手机的管理权限已关闭。"
	if on {
		msg = "这台手机现在可以用 App 管理后台了。"
	}
	if err == nil {
		s.opts.Log.Info("手机的管理权限已在后台修改", "设备", id, "开启", on, "来自", r.RemoteAddr)
	}
	back(w, r, "/devices", msg, err)
}

func (s *Server) deviceRevoke(w http.ResponseWriter, r *http.Request, _ *view) {
	id := strings.TrimSpace(r.PostFormValue("id"))
	ok, err := s.opts.Store.RevokeDevice(r.Context(), id, time.Now().UTC())
	if err == nil && !ok {
		err = device.ErrNotFound
	}
	if err == nil {
		s.opts.Log.Info("手机设备已在后台吊销", "设备", id, "来自", r.RemoteAddr)
	}
	back(w, r, "/devices", "设备已吊销，它的令牌立即失效。", err)
}

func (s *Server) deviceDelete(w http.ResponseWriter, r *http.Request, _ *view) {
	id := strings.TrimSpace(r.PostFormValue("id"))
	err := s.opts.Store.DeleteDevice(r.Context(), id)
	if err == nil {
		s.opts.Log.Info("手机设备及其数据已在后台删除", "设备", id, "来自", r.RemoteAddr)
	}
	back(w, r, "/devices", "设备及它上报的全部数据已删除。", err)
}

func (s *Server) deviceClear(w http.ResponseWriter, r *http.Request, _ *view) {
	id, kind := strings.TrimSpace(r.PostFormValue("id")), strings.TrimSpace(r.PostFormValue("kind"))
	n, err := s.opts.Store.DeleteData(r.Context(), id, kind)
	if err == nil {
		s.opts.Log.Info("手机设备的数据已在后台清除", "设备", id, "种类", kind, "条数", n, "来自", r.RemoteAddr)
	}
	back(w, r, "/devices", "已清除上报的数据。", err)
}
