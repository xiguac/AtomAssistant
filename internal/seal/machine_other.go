//go:build !linux

package seal

import (
	"fmt"
	"os"
	"runtime"
)

// readMachineParts 在 Linux 之外的系统上只用主机名和处理器架构。
//
// 正式部署的目标是 Linux；这里只为了在开发机上能跑起来，绑定强度远不如 Linux 上的实现。
func readMachineParts() (map[string]string, error) {
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("seal: 读不到主机名: %w", err)
	}
	return map[string]string{"host": host, "arch": runtime.GOOS + "/" + runtime.GOARCH}, nil
}
