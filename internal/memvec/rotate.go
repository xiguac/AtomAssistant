package memvec

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"math/rand/v2"
)

// 存进向量库的向量先乘一个由数据密钥派生的随机正交矩阵。
//
// 正交变换不改变向量之间的夹角，检索结果和不转时完全一样；但拿到向量库的人
// 没有数据密钥，就没法把向量和公开的模型对上，也就没法拿常见的词句去比对出记忆的内容。
// 这是混淆，不是严格意义上的加密：攻击者如果另有足够多已知原文的记忆，可以解出这个矩阵。
// 记忆的原文本身不进向量库，只存记忆编号。

// rotation 是一个 dim×dim 的正交矩阵，按行存放。
type rotation struct {
	dim  int
	rows [][]float64
	// fp 是它的指纹（连同模型名一起算），用作向量库集合名的一部分：
	// 换了密钥、换了模型，或者生成方式变了，就落到一个新的集合里重新建，不会和旧向量混用。
	fp string
}

// newRotation 用 key 派生出一个 dim 维的随机正交矩阵：同样的 key 和 dim 总得到同一个。
func newRotation(key []byte, dim int, model string) *rotation {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("memvec rotation v1"))
	var seed [32]byte
	copy(seed[:], mac.Sum(nil))
	rng := rand.NewChaCha8(seed)
	// 均匀分布转成正态分布（Box–Muller）。只用 ChaCha8 的原始输出，不依赖标准库里可能调整的分布实现。
	uniform := func() float64 { return (float64(rng.Uint64()>>11) + 0.5) / (1 << 53) }
	normal := func() float64 {
		return math.Sqrt(-2*math.Log(uniform())) * math.Cos(2*math.Pi*uniform())
	}
	rows := make([][]float64, dim)
	for i := range rows {
		rows[i] = make([]float64, dim)
		for j := range rows[i] {
			rows[i][j] = normal()
		}
	}
	// 修正的 Gram–Schmidt 正交化：正态随机矩阵正交化后是均匀分布的随机正交矩阵。
	// 做两遍，把浮点误差压下去。
	for pass := 0; pass < 2; pass++ {
		for i := range rows {
			for k := 0; k < i; k++ {
				d := dot(rows[i], rows[k])
				for j := range rows[i] {
					rows[i][j] -= d * rows[k][j]
				}
			}
			n := math.Sqrt(dot(rows[i], rows[i]))
			for j := range rows[i] {
				rows[i][j] /= n
			}
		}
	}
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte{0})
	var buf [4]byte
	for _, r := range rows {
		for _, v := range r {
			binary.LittleEndian.PutUint32(buf[:], math.Float32bits(float32(v)))
			h.Write(buf[:])
		}
	}
	return &rotation{dim: dim, rows: rows, fp: hex.EncodeToString(h.Sum(nil)[:6])}
}

// apply 返回转过的向量。
func (r *rotation) apply(v []float32) []float32 {
	out := make([]float32, r.dim)
	for i, row := range r.rows {
		var s float64
		for j, x := range v {
			s += row[j] * float64(x)
		}
		out[i] = float32(s)
	}
	return out
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}
