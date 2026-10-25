package tundevice

import (
	"errors"
	"fmt"
	"io"

	"github.com/libp2p/go-libp2p/core/network"
)

// candidateWins 优先排除退役流，再按发起方仲裁；调用方持有 r.mu。
func (r *Router) candidateWins(existing, candidate *peerFlow) bool {
	if existing == nil {
		return true
	}
	if existing.peer != candidate.peer {
		return false
	}
	if existing.retired.Load() {
		return true
	}
	if candidate.initiator != existing.initiator {
		return candidate.initiator < existing.initiator
	}
	// 同发起方仅接收端接受重拨：发起端 single-flight 已先淘汰旧流，
	// 接收端旧 pump 可能还没退出。本机拨号结果不得反过来顶掉健康在位流。
	// 依赖同一发起端按代重拨；不支持跨连接的入向 handler 任意代际重排。
	return candidate.initiator == candidate.peer && candidate.initiator != r.selfID
}

func streamReadExitReason(stream network.Stream, err error) string {
	connection := "unknown"
	if conn := stream.Conn(); conn != nil {
		connection = conn.ID()
	}
	if errors.Is(err, io.EOF) {
		return fmt.Sprintf("closed by peer: %v (conn=%s stream=%s)", err, connection, stream.ID())
	}
	return fmt.Sprintf("read failed: %v (conn=%s stream=%s)", err, connection, stream.ID())
}
