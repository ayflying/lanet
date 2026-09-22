package serverless

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
)

const controlMaxConcurrent = 64
const controlPeerBurst = 32
const controlMaxPeers = 4096

type controlWindow struct {
	start time.Time
	count int
}
type controlRateLimiter struct {
	mu     sync.Mutex
	active int
	peers  map[string]controlWindow
}

func (r *controlRateLimiter) allow(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active >= controlMaxConcurrent {
		return false
	}
	if r.peers == nil {
		r.peers = make(map[string]controlWindow)
	}
	w, exists := r.peers[key]
	if !exists && len(r.peers) >= controlMaxPeers {
		for k, v := range r.peers {
			if now.Sub(v.start) >= time.Minute {
				delete(r.peers, k)
			}
		}
		if len(r.peers) >= controlMaxPeers {
			return false
		}
	}
	if now.Sub(w.start) >= time.Minute {
		w = controlWindow{start: now}
	}
	if w.count >= controlPeerBurst {
		return false
	}
	w.count++
	r.peers[key] = w
	r.active++
	return true
}
func (r *controlRateLimiter) release() { r.mu.Lock(); r.active--; r.mu.Unlock() }

// 主协议与兼容入口共享同一逻辑桶，不能换协议 ID 绕过额度。
func (d *Discovery) gateControl(bucket string, next network.StreamHandler) network.StreamHandler {
	return func(s network.Stream) {
		if !d.controlGate.allow(bucket+":"+s.Conn().RemotePeer().String(), time.Now()) {
			_ = s.Reset()
			return
		}
		defer d.controlGate.release()
		next(s)
	}
}

// 额外读取一字节检测超限，且验证整个消息，拒绝合法前缀后的垃圾或第二个 JSON。
func decodeControlJSON(r io.Reader, dst any) error {
	raw, err := io.ReadAll(io.LimitReader(r, maxInfoJSONSize+1))
	if err != nil {
		return err
	}
	if len(raw) > maxInfoJSONSize {
		return fmt.Errorf("serverless: JSON 超过 %d 字节", maxInfoJSONSize)
	}
	if len(raw) == 0 {
		return io.EOF
	}
	return json.Unmarshal(raw, dst)
}

// 返回清理函数，正常完成时解除取消监听，避免后台 context 残留协程。
func bindControlIO(ctx context.Context, s network.Stream) func() {
	_ = s.SetDeadline(infoStreamDeadline(ctx))
	stop := context.AfterFunc(ctx, func() { _ = s.Reset() })
	return func() {
		stop()
		if ctx.Err() != nil {
			_ = s.Reset()
		} else {
			_ = s.Close()
		}
	}
}
