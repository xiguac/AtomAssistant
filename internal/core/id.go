package core

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

// crockfordAlphabet 去掉了 I L O U，避免人工抄写时的歧义。
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID 生成 26 字符的 ULID 风格标识：48 位毫秒时间戳 + 80 位随机数。
//
// 选择自己实现而不是引入 uuid 依赖，是因为这里需要的关键性质是**按时间可排序**：
// 步骤日志、事件、审计记录都依赖 ID 的字典序等于时间序，才能用主键直接分页。
// uuid v4 没有这个性质。
func NewID() string {
	var buf [16]byte
	ms := uint64(time.Now().UTC().UnixMilli())
	binary.BigEndian.PutUint64(buf[:8], ms<<16)
	if _, err := rand.Read(buf[6:]); err != nil {
		// crypto/rand 失败意味着系统熵源不可用，此时任何 ID 都不可信，
		// 继续运行会产生可预测的标识符，属于安全问题，直接 panic。
		panic(fmt.Sprintf("core: 无法读取系统随机数: %v", err))
	}
	return encodeCrockford(buf)
}

func encodeCrockford(b [16]byte) string {
	out := make([]byte, 26)
	// 128 位数据编码成 26 个 5 位分组（共 130 位），高 2 位补零。
	var carry uint16
	bits := 0
	idx := 25
	for i := 15; i >= 0; i-- {
		carry |= uint16(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[idx] = crockfordAlphabet[carry&0x1f]
			idx--
			carry >>= 5
			bits -= 5
		}
	}
	if idx >= 0 {
		out[idx] = crockfordAlphabet[carry&0x1f]
		idx--
	}
	for ; idx >= 0; idx-- {
		out[idx] = crockfordAlphabet[0]
	}
	return string(out)
}
