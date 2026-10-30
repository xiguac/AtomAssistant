"""核对向量服务用的 ONNX 模型与官方权重是否一致。

ONNX 版本是第三方从官方模型转出来的。这里用 numpy 按 BERT 的结构从官方 model.safetensors
直接算一遍，和 onnxruntime 的结果逐个比对；两份独立的实现对得上，才说明转换没有走样。

用法：python3 check.py 模型目录      对不上时退出码 1
"""
import json
import math
import os
import sys

import numpy as np

# 核对用的句子：长短、中英混排、数字都有。
SAMPLES = [
    "客户张总对海鲜过敏",
    "不能吃虾",
    "下周五下午三点与供应商开会，地点三号会议室",
    "报销上限是每月 500 元",
    "我喜欢靠窗的座位",
    "Please book a flight to Shanghai tomorrow morning",
    "把合同 PDF 转成 Word，发给法务的李律师",
    "妈妈的生日是 3 月 12 日",
]


def load_safetensors(path):
    with open(path, "rb") as f:
        n = int.from_bytes(f.read(8), "little")
        header = json.loads(f.read(n))
        base = 8 + n
        out = {}
        for name, meta in header.items():
            if name == "__metadata__":
                continue
            if meta["dtype"] == "I64" and name.endswith("position_ids"):
                continue                      # 位置编号 0..511 的缓冲区，计算时直接用 arange
            if meta["dtype"] != "F32":
                raise SystemExit("只支持 F32 权重，%s 是 %s" % (name, meta["dtype"]))
            a, b = meta["data_offsets"]
            f.seek(base + a)
            arr = np.frombuffer(f.read(b - a), dtype="<f4").reshape(meta["shape"])
            out[name.removeprefix("bert.")] = arr.astype(np.float32)
    return out


def layer_norm(x, w, b, eps):
    mu = x.mean(-1, keepdims=True)
    var = ((x - mu) ** 2).mean(-1, keepdims=True)
    return (x - mu) / np.sqrt(var + eps) * w + b


_erf = np.vectorize(math.erf, otypes=[np.float64])


def gelu(x):
    """BERT 的 gelu 是精确的 erf 形式，不是 tanh 近似。"""
    return (0.5 * x * (1.0 + _erf(x / math.sqrt(2.0)))).astype(np.float32)


def reference(model_dir, ids, mask):
    cfg = json.load(open(os.path.join(model_dir, "config.json"), encoding="utf-8"))
    W = load_safetensors(os.path.join(model_dir, "model.safetensors"))
    eps, H = cfg["layer_norm_eps"], cfg["num_attention_heads"]
    B, T = ids.shape
    x = W["embeddings.word_embeddings.weight"][ids] + \
        W["embeddings.position_embeddings.weight"][np.arange(T)][None] + \
        W["embeddings.token_type_embeddings.weight"][0][None, None]
    x = layer_norm(x, W["embeddings.LayerNorm.weight"], W["embeddings.LayerNorm.bias"], eps)
    D = x.shape[-1]
    hd = D // H
    add = (1.0 - mask[:, None, None, :].astype(np.float32)) * np.float32(-1e9)
    lin = lambda h, p: h @ W[p + ".weight"].T + W[p + ".bias"]
    for i in range(cfg["num_hidden_layers"]):
        p = "encoder.layer.%d." % i
        q = lin(x, p + "attention.self.query").reshape(B, T, H, hd).transpose(0, 2, 1, 3)
        k = lin(x, p + "attention.self.key").reshape(B, T, H, hd).transpose(0, 2, 1, 3)
        v = lin(x, p + "attention.self.value").reshape(B, T, H, hd).transpose(0, 2, 1, 3)
        s = q @ k.transpose(0, 1, 3, 2) / np.float32(math.sqrt(hd)) + add
        s = np.exp(s - s.max(-1, keepdims=True))
        s /= s.sum(-1, keepdims=True)
        a = (s @ v).transpose(0, 2, 1, 3).reshape(B, T, D)
        x = layer_norm(x + lin(a, p + "attention.output.dense"),
                       W[p + "attention.output.LayerNorm.weight"], W[p + "attention.output.LayerNorm.bias"], eps)
        f = lin(gelu(lin(x, p + "intermediate.dense")), p + "output.dense")
        x = layer_norm(x + f, W[p + "output.LayerNorm.weight"], W[p + "output.LayerNorm.bias"], eps)
    cls = x[:, 0, :]
    return cls / np.linalg.norm(cls, axis=1, keepdims=True)


def main(model_dir):
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    from embedd import Embedder
    emb = Embedder(model_dir, threads=2)
    got = emb.embed(SAMPLES)
    enc = emb.tok.encode_batch(SAMPLES)
    ids = np.array([e.ids for e in enc], dtype=np.int64)
    mask = np.array([e.attention_mask for e in enc], dtype=np.int64)
    want = reference(model_dir, ids, mask)

    diff = float(np.abs(got - want).max())
    cos = float((got * want).sum(1).min())
    print("ONNX 与官方权重逐元素最大差 %.2e，最小余弦相似度 %.8f（%d 句，%d 维）" % (diff, cos, len(SAMPLES), got.shape[1]))
    sim = got @ got.T
    print("相似度示例：「%s」与「%s」%.3f；与「%s」%.3f" % (SAMPLES[0], SAMPLES[1], sim[0, 1], SAMPLES[3], sim[0, 3]))
    if diff > 1e-4 or cos < 0.99999:
        print("不通过：ONNX 版本与官方权重算出的向量对不上")
        return 1
    print("通过")
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("用法: python3 check.py 模型目录")
    sys.exit(main(sys.argv[1]))
