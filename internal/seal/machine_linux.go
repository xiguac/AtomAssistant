//go:build linux

package seal

import (
	"bufio"
	"os"
	"strings"
)

// readMachineParts 读取这台机器的硬件特征。
//
// 只用任何账号都读得到的来源：常驻服务用的是受限的服务账号，运维命令常常以 root 运行，
// 两边必须算出同一个结果。主板序列号、product_uuid 这些只有 root 读得到，所以不用。
func readMachineParts() (map[string]string, error) {
	parts := map[string]string{}
	for name, path := range map[string]string{
		// 系统安装时生成，重装系统会变。
		"machine-id": "/etc/machine-id",
		"vendor":     "/sys/class/dmi/id/sys_vendor",
		"product":    "/sys/class/dmi/id/product_name",
		"board":      "/sys/class/dmi/id/board_name",
	} {
		if raw, err := os.ReadFile(path); err == nil {
			parts[name] = strings.TrimSpace(string(raw))
		}
	}
	if parts["machine-id"] == "" {
		if raw, err := os.ReadFile("/var/lib/dbus/machine-id"); err == nil {
			parts["machine-id"] = strings.TrimSpace(string(raw))
		}
	}
	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
				parts["cpu"] = strings.TrimSpace(v)
				break
			}
		}
		_ = f.Close()
	}
	return parts, nil
}
