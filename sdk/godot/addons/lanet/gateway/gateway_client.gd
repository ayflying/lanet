class_name LanetGatewayClient
extends Node

## ws-gateway 客户端：玩家 P2P 联机的传输层。
##
## 职责：WebSocket 连接、鉴权、心跳保活、开流（玩家互连桥接）、
## 入向流接收、帧收发与流生命周期管理。
##
## 典型用法（一般游戏直接用 LanetManager 高层 API，不必碰本类）：
##   var c := LanetGatewayClient.new()
##   add_child(c)
##   c.authenticated.connect(func(info): print("入网成功 ", info))
##   c.connect_to_gateway("ws://host:8700/gateway", "INVITE", "alice")

signal authenticated(info: Dictionary)      # 鉴权通过 {virtual_ip, peer_id, group, mode}
signal auth_failed(reason: String)          # 鉴权失败
signal disconnected()                       # 连接断开（含网络错误）
signal stream_opened(stream)                # 对端开来一条流（LanetGatewayStream）

const FRAME := preload("res://addons/lanet/gateway/gateway_frame.gd")
const STREAM := preload("res://addons/lanet/gateway/gateway_stream.gd")
const PING_INTERVAL := 25.0       # 心跳间隔（秒），网关空闲超时 60s
const PONG_TIMEOUT := 10.0        # 心跳应答超时（秒）

var my_name: String = ""          # 本端鉴权名（玩家名）
var is_connected_gateway := false # 鉴权是否已通过

var _ws: WebSocketPeer = null
var _url := ""
var _invite := ""
var _streams := {}                # stream_id → 流对象
var _next_stream_id := 1          # 本端主动开流的 ID（奇数）；入向流由网关分配
var _ping_accum := 0.0
var _pong_wait := false
var _pong_accum := 0.0
var _auth_done := false
var _auth_sent := false     # 防止重复发送 AUTH 帧


## 连接网关并鉴权。name 为玩家名（网关内全局唯一，重名会被拒绝）。
func connect_to_gateway(url: String, invite_code: String, player_name: String) -> Error:
	if _ws != null:
		return ERR_ALREADY_IN_USE
	_url = url
	_invite = invite_code
	my_name = player_name
	_auth_done = false
	_auth_sent = false
	_ws = WebSocketPeer.new()
	return _ws.connect_to_url(url)


## 断开连接并清理所有流。
func disconnect_from_gateway() -> void:
	_cleanup_streams()
	if _ws != null:
		_ws.close()
		_ws = null
	is_connected_gateway = false


## 开一条到目标玩家的流（玩家互连）。target 为对方鉴权名。
## 返回流对象；开流结果经流上的 dial_ok/dial_failed 信号通知。
func dial_player(target: String, protocol: String = "/game/1.0.0"):
	var sid := _next_stream_id
	_next_stream_id += 2
	var st = STREAM.new(self, sid)
	st.protocol = protocol
	_streams[sid] = st
	var payload := JSON.stringify({"peer": target, "protocol": protocol}).to_utf8_buffer()
	_send_frame(FRAME.TYPE_DIAL, sid, payload)
	return st


## 是否存在到某玩家的活跃流。
func has_stream_to(player_name: String) -> bool:
	for sid in _streams:
		var st = _streams[sid]
		if st.remote_peer == player_name and not st._finished:
			return true
	return false


# ---- 内部：帧发送 ----

func _send_frame(type: int, stream_id: int, payload: PackedByteArray) -> void:
	if _ws == null:
		return
	var buf := FRAME.marshal(type, stream_id, payload)
	_ws.send(buf, WebSocketPeer.WRITE_MODE_BINARY)


func _send_data(stream_id: int, chunk: PackedByteArray) -> Error:
	if _ws == null or not is_connected_gateway:
		return ERR_CONNECTION_ERROR
	_send_frame(FRAME.TYPE_DATA, stream_id, chunk)
	return OK


func _send_close(stream_id: int) -> void:
	_send_frame(FRAME.TYPE_CLOSE, stream_id, PackedByteArray())


func _send_reset(stream_id: int) -> void:
	_send_frame(FRAME.TYPE_RESET, stream_id, PackedByteArray())


func _send_ping() -> void:
	_send_frame(FRAME.TYPE_PING, 0, PackedByteArray())


## 发送鉴权帧（WS 建立后的首帧）。
func _send_auth() -> void:
	var payload := JSON.stringify({
		"invite_code": _invite,
		"name": my_name,
		"mode": "client",
	}).to_utf8_buffer()
	_send_frame(FRAME.TYPE_AUTH, 0, payload)


# ---- 主循环 ----

func _process(delta: float) -> void:
	if _ws == null:
		return
	_ws.poll()
	match _ws.get_ready_state():
		WebSocketPeer.STATE_OPEN:
			if not _auth_sent:
				_auth_sent = true
				_send_auth()
			_drain_packets()
			_tick_heartbeat(delta)
		WebSocketPeer.STATE_CLOSED:
			var had_auth := is_connected_gateway
			var close_code := _ws.get_close_code()
			_cleanup_streams()
			_ws = null
			is_connected_gateway = false
			if had_auth or _auth_done:
				disconnected.emit()
			else:
				auth_failed.emit("连接失败: close_code=%d" % close_code)


func _drain_packets() -> void:
	while _ws.get_available_packet_count() > 0:
		var pkt := _ws.get_packet()
		var frame := FRAME.unmarshal(pkt)
		if frame.is_empty():
			continue
		_handle_frame(frame)


func _handle_frame(frame: Dictionary) -> void:
	var type: int = frame["type"]
	var sid: int = frame["stream_id"]
	var payload: PackedByteArray = frame["payload"]
	match type:
		FRAME.TYPE_AUTH_OK:
			_auth_done = true
			is_connected_gateway = true
			var info := FRAME.payload_json(frame)
			authenticated.emit(info)
		FRAME.TYPE_AUTH_ERR:
			_auth_done = true
			var err_info := FRAME.payload_json(frame)
			var reason: String = str(err_info.get("error", "鉴权失败"))
			_cleanup_streams()
			_ws.close()
			auth_failed.emit(reason)
		FRAME.TYPE_DIAL_OK:
			# 本端主动开的流已建立（桥接完成）。
			var st_ok = _streams.get(sid)
			if st_ok != null:
				st_ok._mark_dial_ok()
		FRAME.TYPE_DIAL_ERR:
			var st_err = _streams.get(sid)
			if st_err != null:
				_streams.erase(sid)
				st_err._mark_dial_failed(payload.get_string_from_utf8())
		FRAME.TYPE_DATA:
			var st2 = _streams.get(sid)
			if st2 != null:
				st2._deliver(payload)
		FRAME.TYPE_CLOSE:
			var st3 = _streams.get(sid)
			if st3 != null:
				st3._mark_eof()
		FRAME.TYPE_RESET:
			var st4 = _streams.get(sid)
			if st4 != null:
				_streams.erase(sid)
				st4._mark_reset()
		FRAME.TYPE_PONG:
			_pong_wait = false
		FRAME.TYPE_STREAM_OPEN:
			# 对端（或网格内节点）开来一条流：登记并通知。
			var meta := FRAME.payload_json(frame)
			var st5 = STREAM.new(self, sid)
			st5.protocol = str(meta.get("protocol", ""))
			st5.remote_peer = str(meta.get("remote_peer", ""))
			st5.is_inbound = true
			_streams[sid] = st5
			stream_opened.emit(st5)
		_:
			pass # 未知类型忽略（前向兼容）


func _tick_heartbeat(delta: float) -> void:
	if not is_connected_gateway:
		return
	if _pong_wait:
		_pong_accum += delta
		if _pong_accum > PONG_TIMEOUT:
			# 心跳超时：主动断开，触发 disconnected，上层可重连。
			_ws.close(1006, "心跳超时")
		return
	_ping_accum += delta
	if _ping_accum >= PING_INTERVAL:
		_ping_accum = 0.0
		_pong_wait = true
		_pong_accum = 0.0
		_send_ping()


func _cleanup_streams() -> void:
	for sid in _streams:
		var st = _streams[sid]
		st._mark_reset()
	_streams.clear()

