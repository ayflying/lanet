class_name LanetGatewayFrame
extends RefCounted

## ws-gateway 二进制帧编解码。
##
## 帧布局（与 Go pkg/gatewayproto、C#、JS 三端字节级一致）：
##   [type:1 字节][streamID:4 字节大端][payload 长度:4 字节大端][payload]
##
## 一条二进制 WebSocket 消息即一个帧。

# 帧类型。
const TYPE_AUTH := 0x01        # c→g  鉴权：JSON {"invite_code","name","mode"}
const TYPE_AUTH_OK := 0x02     # g→c  鉴权通过：JSON {"virtual_ip","peer_id","group","mode"}
const TYPE_AUTH_ERR := 0x03    # g→c  鉴权失败：JSON {"error"}
const TYPE_DIAL := 0x04        # c→g  开流：JSON {"ip","port","protocol","peer"}
const TYPE_DIAL_OK := 0x05     # g→c  开流成功：JSON {"via_relay":bool}
const TYPE_DIAL_ERR := 0x06    # g→c  开流失败：payload 为错误文本
const TYPE_DATA := 0x07        # 双向  流数据
const TYPE_CLOSE := 0x08       # 双向  半关闭写端（对端读到 EOF）
const TYPE_RESET := 0x09       # 双向  强制中止流
const TYPE_PING := 0x0A        # c→g  心跳（payload 原样回 Pong）
const TYPE_PONG := 0x0B        # g→c  心跳应答
const TYPE_STREAM_OPEN := 0x0C # g→c  入向流：JSON {"protocol","remote_peer"}

# 连接模式。
const MODE_CLIENT := "client"  # 主动开流（游戏客户端用这个）
const MODE_SERVICE := "service" # 接收网格内入向流（网关仅允许一个）

const HEADER_SIZE := 9


## 编码一帧为 PackedByteArray（可直接作为 WS 二进制消息发送）。
## 注意：GDScript 的 encode_u32 是小端，而协议要求大端——手写字节序。
static func marshal(type: int, stream_id: int, payload: PackedByteArray) -> PackedByteArray:
	var buf := PackedByteArray()
	buf.resize(HEADER_SIZE + payload.size())
	buf[0] = type
	buf[1] = (stream_id >> 24) & 0xFF
	buf[2] = (stream_id >> 16) & 0xFF
	buf[3] = (stream_id >> 8) & 0xFF
	buf[4] = stream_id & 0xFF
	var n := payload.size()
	buf[5] = (n >> 24) & 0xFF
	buf[6] = (n >> 16) & 0xFF
	buf[7] = (n >> 8) & 0xFF
	buf[8] = n & 0xFF
	for i in n:
		buf[HEADER_SIZE + i] = payload[i]
	return buf


## 编码 JSON 载荷帧。
static func marshal_json(type: int, stream_id: int, data: Dictionary) -> PackedByteArray:
	var json_bytes := JSON.stringify(data).to_utf8_buffer()
	return marshal(type, stream_id, json_bytes)


## 解码一帧。返回 {"type": int, "stream_id": int, "payload": PackedByteArray}；
## 数据不完整或非法时返回空字典。
static func unmarshal(data: PackedByteArray) -> Dictionary:
	if data.size() < HEADER_SIZE:
		return {}
	var type: int = data[0]
	var stream_id: int = (data[1] << 24) | (data[2] << 16) | (data[3] << 8) | data[4]
	var length: int = (data[5] << 24) | (data[6] << 16) | (data[7] << 8) | data[8]
	if data.size() - HEADER_SIZE < length:
		return {}
	var payload := data.slice(HEADER_SIZE, HEADER_SIZE + length)
	return {"type": type, "stream_id": stream_id, "payload": payload}


## 解码 JSON 载荷。
static func payload_json(frame: Dictionary) -> Dictionary:
	var payload: PackedByteArray = frame["payload"]
	if payload.is_empty():
		return {}
	var parsed = JSON.parse_string(payload.get_string_from_utf8())
	return parsed if parsed is Dictionary else {}
