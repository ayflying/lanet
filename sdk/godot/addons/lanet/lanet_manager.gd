class_name LanetManager
extends Node

## 玩家 P2P 联机高层 API（自动加载单例）。
##
## 房间制模型：一人 create_room 成为房主，其他人 join_room 加入。
## 消息通道与 @rpc 通道分离：
##   - send_to / broadcast + data_received：JSON 消息（独立聊天流，简单可靠）；
##   - make_multiplayer_peer()：Godot 原生 MultiplayerAPI / @rpc（位置同步等）。
##
##   Lanet.player_joined.connect(...)
##   Lanet.create_room("ws://host:8700/gateway", "INVITE", "alice")
##   Lanet.send_to("bob", {"hp": 90})

signal room_joined(info: Dictionary)         # 入房完成 {role, my_name, host}
signal room_error(reason: String)            # 入房/连接失败
signal room_left()                           # 已离开房间（主动或断线）
signal player_joined(uid: int, player_name: String)
signal player_left(uid: int)
signal data_received(msg: Dictionary)        # {"from": 玩家名, "data": Dictionary}

const PROTO_CHAT := "/game/chat/1.0.0"
const GatewayClientScript := preload("res://addons/lanet/gateway/gateway_client.gd")
const MultiplayerPeerScript := preload("res://addons/lanet/multiplayer/lanet_multiplayer_peer.gd")

# 内部消息键（走聊天流，不暴露给 data_received）。
const KEY_ROSTER := "_lanet_roster"      # 全量名册：[[uid, name], ...]
const KEY_ROSTER_ADD := "_lanet_roster_add" # 增量：{"uid":X,"name":N}

var client = null                # LanetGatewayClient
var peer = null                  # LanetMultiplayerPeer
var is_host := false
var my_name := ""

var _chat_streams := {}        # player_name → 流对象（聊天流池）
var _uid_names := {}           # uid → 玩家名（roster 同步，guest 也持有全表）
var _mounted_mp := false       # 是否已把本 SDK 的 peer 挂到 MultiplayerAPI


## 创建房间（房主）。等待鉴权完成后进入 host 状态。
func create_room(gateway_url: String, invite_code: String, player_name: String) -> void:
	_leave_quiet()
	is_host = true
	my_name = player_name
	client = _ensure_client()
	client.authenticated.connect(_on_authenticated_host, CONNECT_ONE_SHOT)
	client.auth_failed.connect(_on_auth_failed, CONNECT_ONE_SHOT)
	var err = client.connect_to_gateway(gateway_url, invite_code, player_name)
	if err != OK:
		room_error.emit("连接网关失败: " + error_string(err))


## 加入房主的房间。host_name 为房主的玩家名。
func join_room(gateway_url: String, invite_code: String, player_name: String, host_name: String) -> void:
	_leave_quiet()
	is_host = false
	my_name = player_name
	client = _ensure_client()
	client.authenticated.connect(_on_authenticated_guest.bind(host_name), CONNECT_ONE_SHOT)
	client.auth_failed.connect(_on_auth_failed, CONNECT_ONE_SHOT)
	var err = client.connect_to_gateway(gateway_url, invite_code, player_name)
	if err != OK:
		room_error.emit("连接网关失败: " + error_string(err))


## 离开房间并断开网关。
func leave_room() -> void:
	_leave_quiet()
	room_left.emit()


## 发 JSON 消息给指定玩家（玩家名）。返回错误码。
func send_to(player_name: String, data: Dictionary) -> Error:
	var st = _chat_stream_to(player_name)
	if st == null:
		return ERR_DOES_NOT_EXIST
	var body := JSON.stringify(data).to_utf8_buffer()
	var buf := PackedByteArray()
	buf.resize(4 + body.size())
	buf.encode_u32(0, body.size())
	for i in body.size():
		buf[4 + i] = body[i]
	return st.send_data(buf)


## 广播 JSON 消息给房间内所有其他玩家。
func broadcast(data: Dictionary) -> Error:
	var err := OK
	for player_name in _chat_streams:
		var e := send_to(player_name, data)
		if e != OK:
			err = e
	return err


## 把当前房间接入 Godot 原生 MultiplayerAPI（@rpc）。
## 入房成功后会自动调用；显式调用用于重新挂接（幂等）。
func make_multiplayer_peer():
	if peer == null:
		return null
	multiplayer.multiplayer_peer = peer
	_mounted_mp = true
	return peer


## 玩家名 → 房间 uid（MultiplayerAPI 语境）。未知返回 -1。
func uid_of(player_name: String) -> int:
	if peer == null:
		return -1
	if is_host:
		for uid in peer._peers:
			if str(peer._peers[uid]["name"]) == player_name:
				return uid
		return -1
	# guest 视角：房主恒为 SERVER_ID；其他玩家查房主同步下来的名册。
	if player_name == peer.host_name:
		return MultiplayerPeer.TARGET_PEER_SERVER
	for uid in _uid_names:
		if str(_uid_names[uid]) == player_name:
			return uid
	return -1


# ---- 内部 ----

func _chat_stream_to(player_name: String):
	if client == null or not client.is_connected_gateway:
		return null
	if _chat_streams.has(player_name):
		return _chat_streams[player_name]
	var st = client.dial_player(player_name, PROTO_CHAT)
	st.data_received.connect(_on_chat_bytes.bind(st)) # 主动开的流同样要接数据回调
	st.dial_failed.connect(func(_r: String) -> void:
		_chat_streams.erase(player_name))
	_chat_streams[player_name] = st
	return st


func _on_chat_inbound(st) -> void:
	if st.protocol != PROTO_CHAT:
		return
	st.data_received.connect(_on_chat_bytes.bind(st))
	st.eof.connect(func() -> void: _chat_streams.erase(st.remote_peer))
	st.reset_received.connect(func() -> void: _chat_streams.erase(st.remote_peer))
	if not st.remote_peer.is_empty():
		# 入向聊天流也要入池：增量名册广播与 broadcast 都依赖这个池子，
		# 否则 host 只推得进入房瞬间的全量、后续加入者永远收不到。
		_chat_streams[st.remote_peer] = st
	if is_host:
		# 新 guest 的聊天流建立：把当前全量名册推给它（之后增量由
		# player_meta_joined 广播），guest 由此获得其他玩家的 uid。
		_send_chat_frame(st, {KEY_ROSTER: _roster_snapshot()})


## 当前 uid↔名字 名册快照。
func _roster_snapshot() -> Array:
	var roster: Array = []
	for uid in _uid_names:
		roster.append([uid, _uid_names[uid]])
	return roster


func _send_chat_frame(st, data: Dictionary) -> void:
	var body := JSON.stringify(data).to_utf8_buffer()
	var buf := PackedByteArray()
	buf.resize(4 + body.size())
	buf.encode_u32(0, body.size())
	for i in body.size():
		buf[4 + i] = body[i]
	st.send_data(buf)


func _on_chat_bytes(chunk: PackedByteArray, st) -> void:
	# 聊天流分帧：[len:4][JSON]。
	var acc: PackedByteArray = st.get_meta("rbuf") if st.has_meta("rbuf") else PackedByteArray()
	acc.append_array(chunk)
	while acc.size() >= 4:
		var length := acc.decode_u32(0)
		if acc.size() < 4 + length:
			break
		var parsed = JSON.parse_string(acc.slice(4, 4 + length).get_string_from_utf8())
		acc = acc.slice(4 + length)
		if parsed is Dictionary:
			_handle_chat_message(parsed, st)
	st.set_meta("rbuf", acc)


func _handle_chat_message(data: Dictionary, st) -> void:
	# 内部协议消息。
	if data.has(KEY_ROSTER):
		for entry in data[KEY_ROSTER]:
			if entry is Array and entry.size() == 2:
				_apply_roster_entry(int(entry[0]), str(entry[1]))
		return
	if data.has(KEY_ROSTER_ADD):
		var add: Dictionary = data[KEY_ROSTER_ADD]
		_apply_roster_entry(int(add.get("uid", 0)), str(add.get("name", "")))
		return
	data_received.emit({"from": st.remote_peer, "data": data})


func _apply_roster_entry(uid: int, player_name: String) -> void:
	if uid == multiplayer_uid() or player_name == my_name:
		return
	if _uid_names.get(uid, "") == player_name:
		return
	_uid_names[uid] = player_name
	# 注意：名册只做「uid↔名字」映射与 player_joined 事件。
	# MultiplayerAPI 里的玩家声明由引擎的 SYS/ADD_PEER 机制自动完成。
	player_joined.emit(uid, player_name)


## 本端在 MultiplayerAPI 里的 uid（未入房返回 -1）。
func multiplayer_uid() -> int:
	if peer == null:
		return -1
	return peer._my_uid


func _ensure_client():
	if client != null:
		client.disconnect_from_gateway()
		client.queue_free()
	client = GatewayClientScript.new()
	add_child(client)
	client.stream_opened.connect(_on_chat_inbound)
	client.disconnected.connect(func() -> void:
		if peer == null:
			room_error.emit("与网关的连接已断开")
		else:
			room_left.emit())
	return client


func _setup_peer_signals() -> void:
	peer.player_meta_joined.connect(func(uid: int, name: String) -> void:
		_uid_names[uid] = name
		player_joined.emit(uid, name)
		if is_host:
			# 名册增量广播：已有聊天流的玩家即时获知新成员。
			var add := {KEY_ROSTER_ADD: {"uid": uid, "name": name}}
			for player_name in _chat_streams:
				_send_chat_frame(_chat_streams[player_name], add))
	peer.player_meta_left.connect(func(uid: int) -> void:
		_uid_names.erase(uid)
		player_left.emit(uid))
	peer.server_disconnected.connect(func() -> void:
		room_left.emit())


func _on_authenticated_host(_info: Dictionary) -> void:
	peer = MultiplayerPeerScript.new()
	_setup_peer_signals()
	var err = peer.host_room(client)
	if err != OK:
		room_error.emit("开局失败: " + error_string(err))
		return
	# 名册含房主自己：guest 收到的全量快照由此获得 host 的 uid↔名字。
	_uid_names[MultiplayerPeer.TARGET_PEER_SERVER] = my_name
	make_multiplayer_peer() # 房主同样接入 MultiplayerAPI（@rpc / 引擎 ADD_PEER 广播）
	room_joined.emit({"role": "host", "my_name": my_name})

func _on_authenticated_guest(info: Dictionary, host_name: String) -> void:
	peer = MultiplayerPeerScript.new()
	_setup_peer_signals()
	var err = peer.join_room(client, host_name)
	if err != OK:
		room_error.emit("加入房间失败: " + error_string(err))
		return
	await peer.host_ready # 等到房主的桥接流建立（否则过早挂接会丢包）
	_chat_stream_to(host_name) # 建立与房主的聊天流（名册同步 + send_to 通道）
	make_multiplayer_peer() # 入房即接入 MultiplayerAPI（@rpc 可用）
	peer.announce_ready() # 挂接之后再通知引擎「房主已连接」
	# 其他玩家的存在感由引擎自动同步：房主的 SceneMultiplayer 在 admit 每个
	# guest 时广播 SYS/ADD_PEER，guest 无需（也不应）手动声明——手动声明
	# 会与引擎广播双重 admit，破坏 SceneCacheInterface 的路径缓存握手。
	room_joined.emit({"role": "guest", "host": host_name,
		"my_name": my_name, "virtual_ip": str(info.get("virtual_ip", ""))})


func _on_auth_failed(reason: String) -> void:
	room_error.emit(reason)


func _leave_quiet() -> void:
	if peer != null:
		peer.close_room()
		peer = null
	if _mounted_mp:
		_mounted_mp = false
		if multiplayer != null:
			multiplayer.multiplayer_peer = null # 摘掉本 SDK 的 peer（不动其他实现）
	_chat_streams.clear()
	_uid_names.clear()
	if client != null:
		client.disconnect_from_gateway()
		client.queue_free()
		client = null
