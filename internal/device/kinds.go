package device

// Kind 是一种数据的登记：手机上的 App 和服务端共用这张表。
// 新增一种数据时，两边要同时加（App 里的 Kind 枚举见 ios/AtomAssistant/Models/DataKind.swift）。
type Kind struct {
	ID    string
	Label string
	// PerUse 为真表示每次采集都要用户在 App 里点同意：只能挂在一条授权需求上上报，
	// 设备自己不能主动上报。位置、详细的健康记录这类最私人的数据属于此类。
	PerUse bool
	// Text 为真表示数据里含有别人写的文字（歌名、应用名、Wi-Fi 名等），
	// 助手读到它时按外部内容对待，不当指令。
	Text bool
	// Role 是这种数据的用途：助手规划时主动去读的（RoleDecision），还是先存着、以后再定用途的（RoleArchive）。
	Role string
	// Server 为真表示这种数据是服务器自己产生的（比如从请求来源 IP 推断的归属地），设备不能上报。
	Server bool
}

const (
	// RoleDecision 辅助决策：规划行动时用得上，助手会主动查。
	RoleDecision = "decision"
	// RoleArchive 先存着：先获取下来，用途以后再定。
	RoleArchive = "archive"
)

// Kinds 是全部已知的数据种类，顺序即后台里显示的顺序。
var Kinds = []Kind{
	{ID: "device.status", Label: "设备状态（电量、充电、亮度、音量与音频输出、低电量模式）", Role: RoleDecision},
	{ID: "device.system", Label: "系统信息（型号、系统版本、构建号、屏幕、语言与时区）", Role: RoleArchive},
	{ID: "device.network", Label: "网络状态（连接类型、是否计费、蜂窝制式）", Role: RoleDecision},
	{ID: "location.coarse", Label: "粗略位置（精确位置在手机上随机偏移 10–20 公里后上报）", Role: RoleDecision},
	{ID: "motion.pedometer", Label: "计步（步数、距离、爬楼）", Role: RoleDecision},
	{ID: "motion.activity", Label: "运动状态（静止、步行、跑步、骑行、驾车）", Role: RoleDecision},
	{ID: "focus.status", Label: "专注模式是否开启", Role: RoleDecision},
	{ID: "ip.region", Label: "公网 IP 归属地（城市级；服务器按请求来源 IP 推断，手机不用上报）", Role: RoleDecision, Server: true},
	{ID: "probe.report", Label: "能力探测报告（哪些系统接口在这台手机上可用）", Text: true, Role: RoleArchive},

	{ID: "location.now", Label: "当前位置", PerUse: true, Role: RoleDecision},
	{ID: "location.track", Label: "一段时间内的位置轨迹", PerUse: true, Role: RoleArchive},
	{ID: "bluetooth.scan", Label: "扫描附近的蓝牙设备（你打开 App 同意后扫描）", PerUse: true, Text: true, Role: RoleDecision},
	{ID: "network.scan", Label: "扫描当前局域网里的设备（你打开 App 同意后扫描）", PerUse: true, Text: true, Role: RoleDecision},
	{ID: "motion.query", Label: "指定时间段的计步与运动历史", PerUse: true, Role: RoleArchive},
}

// KindByID 按编号找一种数据。
func KindByID(id string) (Kind, bool) {
	for _, k := range Kinds {
		if k.ID == id {
			return k, true
		}
	}
	return Kind{}, false
}
