"""本机向量服务：把文字变成向量，给记忆检索用。

模型是 BAAI/bge-small-zh-v1.5（BERT，4 层，512 维），用 onnxruntime 跑它的 ONNX 版本，
分词用官方的 tokenizer.json。取 [CLS] 位置的输出再做 L2 归一化，与模型的 1_Pooling 配置一致。
ONNX 版本和官方权重是否一致，由 check.py 用另一份独立的 numpy 实现核对（deploy/embed.sh 安装时会跑）。

接口（只听本机）：
  POST /embed   {"texts": ["...", ...]}  ->  {"dim": 512, "vectors": [[...], ...]}
  GET  /healthz                          ->  {"ok": true, "model": "..."}

用法：python3 embedd.py --model 模型目录 [--host 127.0.0.1] [--port 8766] [--threads 2]
"""
import argparse
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np

MAX_TEXTS = 64        # 一次最多编码多少段
MAX_TOKENS = 512      # 模型的位置编码上限，超出的截断
MAX_BODY = 4 << 20    # 请求体上限


class Embedder:
    def __init__(self, model_dir, threads):
        import onnxruntime as ort
        from tokenizers import Tokenizer

        self.name = os.path.basename(os.path.normpath(model_dir))
        self.tok = Tokenizer.from_file(os.path.join(model_dir, "tokenizer.json"))
        self.tok.enable_truncation(MAX_TOKENS)
        self.tok.enable_padding(pad_id=0, pad_token="[PAD]")
        opts = ort.SessionOptions()
        opts.intra_op_num_threads = threads
        opts.inter_op_num_threads = 1
        self.sess = ort.InferenceSession(os.path.join(model_dir, "model.onnx"), opts,
                                         providers=["CPUExecutionProvider"])
        self.inputs = {i.name for i in self.sess.get_inputs()}
        # onnxruntime 的会话可以并发调用，但这台机器核少，排队跑反而稳。
        self.lock = threading.Lock()

    def embed(self, texts):
        enc = self.tok.encode_batch(texts)
        ids = np.array([e.ids for e in enc], dtype=np.int64)
        mask = np.array([e.attention_mask for e in enc], dtype=np.int64)
        feed = {"input_ids": ids, "attention_mask": mask}
        if "token_type_ids" in self.inputs:
            feed["token_type_ids"] = np.zeros_like(ids)
        with self.lock:
            hidden = self.sess.run(None, feed)[0]          # [B, T, 512]
        cls = hidden[:, 0, :].astype(np.float32)
        cls /= np.linalg.norm(cls, axis=1, keepdims=True).clip(min=1e-12)
        return cls


EMB = None


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def _json(self, code, obj):
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.split("?")[0] == "/healthz":
            self._json(200, {"ok": True, "model": EMB.name})
            return
        self._json(404, {"error": "not found"})

    def do_POST(self):
        if self.path.split("?")[0] != "/embed":
            self._json(404, {"error": "not found"})
            return
        n = int(self.headers.get("Content-Length") or 0)
        if n <= 0 or n > MAX_BODY:
            self._json(400, {"error": "请求体为空或过大"})
            return
        try:
            req = json.loads(self.rfile.read(n))
            texts = req["texts"]
            if not isinstance(texts, list) or not texts or len(texts) > MAX_TEXTS or \
                    not all(isinstance(t, str) for t in texts):
                raise ValueError("texts 要是 1 到 %d 段文字" % MAX_TEXTS)
        except Exception as e:
            self._json(400, {"error": "请求格式不对：%s" % e})
            return
        try:
            vecs = EMB.embed(texts)
        except Exception as e:
            self._json(500, {"error": "编码失败：%s" % e})
            return
        self._json(200, {"dim": int(vecs.shape[1]), "vectors": vecs.tolist()})


def main(argv=None):
    global EMB
    ap = argparse.ArgumentParser(description="本机向量服务")
    ap.add_argument("--model", required=True, help="模型目录（含 model.onnx 与 tokenizer.json）")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8766)
    ap.add_argument("--threads", type=int, default=2)
    a = ap.parse_args(argv)
    EMB = Embedder(a.model, a.threads)
    EMB.embed(["预热"])
    srv = ThreadingHTTPServer((a.host, a.port), Handler)
    print("向量服务已就绪：%s，http://%s:%d" % (EMB.name, a.host, a.port), flush=True)
    try:
        srv.serve_forever()
    finally:
        srv.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
