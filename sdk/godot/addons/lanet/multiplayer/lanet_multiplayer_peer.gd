class_name LanetMultiplayerPeer
extends MultiplayerPeerExtension

## 把 Lanet 玩家互连桥接流适配为 Godot MultiplayerPeer。
##
## 模型（与 ENet 一致的星型拓扑）：
##   - 房主 = server，unique_id 固定为 1（MultiplayerPeer.TARGET_PEER_SERVER）；
##   - 每个加入者与房主之间一条桥接流；@rpc 包由 SceneMultiplayer 编解码，
##     server relay 支持玩家间转发；
##   - 包分帧：[长度:4 字节大端][packet]，握手首帧为同格式的 JSON
##     {"uid":N,"name":"玩家名"}（uid 由加入者随机生成，房主恒为 1）。
##
## 用法：
##   var peer := LanetMultiplayerPeer.new()
##   peer.host_room(gateway_client)             # 房主
##   peer.join_room(gateway_client, "alice")    # 加入 alice 的房间
##   multiplayer.multiplayer_peer = peer

signal player_meta_joined(uid: int, player_name: String) # 握手完成（含玩家名）
signal player_meta_left(uid: int)
signal server_disconnected               # 与房主的连接断开（仅 guest 视角）
signal host_ready                        # guest：到房主的流已建立（可开始收发）

const PROTO_GAME := "/game/1.0.0"
const MAX_PACKET := 1 << 20 # 与网关 WS 读限一致

var _is_host := false
var _my_uid := 0
var _status: int = CONNECTION_DISCONNECTED

# 房主视角的 guest 连接表：uid → 连接对象（内部字典）。
# guest 视角只有 _host_conn 一条到房主的连接。
# 连接对象：{"stream": 流对象, "name": String,
#           "rbuf": PackedByteArray, "handshaked": bool}
var _peers := {}
var _host_conn = null
var host_name := ""                 # guest 视角：房主玩家名（join_room 时记录）
var _client = null                  # LanetGatewayClient
var _target_peer := 0
var _last_packet_peer := 0     # 最近出队的包来源（_get_packet_peer）
var _refuse_new_connections := false
var _incoming: Array = []      # [{data: PackedByteArray, peer: int}]

# 包传输模式：桥接流是可靠有序的 TCP 语义，unreliable 请求按 reliable 发送
# （边界已在 README 声明）。频道不支持，一律 0。


## 以房主身份开局：接收其他玩家的入向流。
func host_room(client) -> Error:
	if client == null or not client.is_connected_gateway:
		return ERR_CONNECTION_ERROR
	_reset_state()
	_client = client
	_is_host = true
	_my_uid = MultiplayerPeer.TARGET_PEER_SERVER
	_client.stream_opened.connect(_on_inbound_stream)
	_status = CONNECTION_CONNECTED
	return OK


## 以玩家身份加入房主的房间（向房主开流并完成握手）。
func join_room(client, host_name: String) -> Error:
	if client == null or not client.is_connected_gateway:
		return ERR_CONNECTION_ERROR
	_reset_state()
	_client = client
	_is_host = false
	self.host_name = host_name
	_my_uid = _random_uid()
	_status = CONNECTION_CONNECTING
	var st = client.dial_player(host_name, PROTO_GAME)
	_host_conn = {"stream": st, "name": host_name, "rbuf": PackedByteArray(), "handshaked": false}
	st.dial_ok.connect(_on_host_dial_ok)
	st.dial_failed.connect(_on_host_dial_failed)
	st.eof.connect(_on_host_gone)
	st.reset_received.connect(_on_host_gone)
	st.data_received.connect(_on_host_bytes)
	return OK


## 退出房间并清理。
func close_room() -> void:
	_reset_state()
	_status = CONNECTION_DISCONNECTED


## 玩家名查询（房主与 guest 都可用；未知 uid 返回空串）。
func player_name_of(uid: int) -> String:
	if _is_server and uid == MultiplayerPeer.TARGET_PEER_SERVER:
		return _client.my_name if _client != null else ""
	var p = _peers.get(uid)
	return str(p["name"]) if p != null else ""


func _reset_state() -> void:
	if _client != null and _client.stream_opened.is_connected(_on_inbound_stream):
		_client.stream_opened.disconnect(_on_inbound_stream)
	for uid in _peers:
		var st = _peers[uid]["stream"]
		if st != null and not st._finished:
			st.reset()
	if _host_conn != null:
		var hs = _host_conn["stream"]
		if hs != null and not hs._finished:
			hs.reset()
	_peers.clear()
	_host_conn = null
	_incoming.clear()
	_target_peer = 0


func _random_uid() -> int:
	# 避开 0/1，1 是房主；碰撞概率极低，房主端重名握手直接拒绝重连。
	return randi_range(2, 0x7FFFFFF0)


# ---- 流事件（信号驱动；切包在数据到达时完成，_poll 只做状态维护）----

func _on_host_dial_ok() -> void:
	# 到房主的流建立：发握手帧。peer_connected 延迟到 announce_ready()
	# ——必须等上层把本 peer 挂入 MultiplayerAPI 之后再发，否则信号丢失。
	_send_handshake(_host_conn["stream"], _my_uid, _client.my_name)
	_status = CONNECTION_CONNECTED
	host_ready.emit()


## guest 在挂接 MultiplayerAPI 之后调用：通知引擎「房主已连接」。
func announce_ready() -> void:
	peer_connected.emit(MultiplayerPeer.TARGET_PEER_SERVER)
	if _host_conn != null and _host_conn.get("handshaked", false):
		player_meta_joined.emit(MultiplayerPeer.TARGET_PEER_SERVER, str(_host_conn["name"]))


func _on_host_dial_failed(reason: String) -> void:
	_status = CONNECTION_DISCONNECTED
	server_disconnected.emit()


func _on_inbound_stream(st) -> void:
	if st.protocol != PROTO_GAME:
		return # 非游戏流（其他协议互不干扰）
	var conn := {"stream": st, "name": "", "rbuf": PackedByteArray(), "handshaked": false}
	# 挂在 _peers 前先握手；用 stream_id 作临时键定位。
	_pending_handshakes[st.stream_id] = conn
	st.data_received.connect(_on_guest_bytes.bind(conn))
	st.eof.connect(_on_stream_gone.bind(conn))
	st.reset_received.connect(_on_stream_gone.bind(conn))


var _pending_handshakes := {} # stream_id → 连接对象（握手未完成）


func _on_guest_bytes(chunk: PackedByteArray, conn: Dictionary) -> void:
	if not conn["handshaked"]:
		var rest = _try_handshake(conn, chunk)
		if rest == null:
			return # 数据不全，继续等
		if _peers.has(conn["pending_uid"]):
			# uid 冲突（极小概率）：拒绝并断开这条流。
			conn["stream"].reset()
			return
		var uid: int = conn["pending_uid"]
		conn["handshaked"] = true
		conn["name"] = conn["pending_name"]
		conn.erase("pending_name")
		conn.erase("pending_uid")
		_peers[uid] = conn
		_pending_handshakes.erase(conn["stream"].stream_id)
		# 回握手：告知 guest 本端 uid（恒为 1）与名字。
		_send_handshake(conn["stream"], MultiplayerPeer.TARGET_PEER_SERVER, _client.my_name)
		# SceneMultiplayer（已挂接）收到 peer_connected 后自动向其他 guest
		# 广播 SYS/ADD_PEER——玩家列表同步完全交给引擎。
		peer_connected.emit(uid)
		player_meta_joined.emit(uid, str(conn["name"]))
		if rest.is_empty():
			return
		chunk = rest # 同一 chunk 中握手后的数据继续按数据帧处理
	_append_fragments(conn, chunk)


func _on_host_bytes(chunk: PackedByteArray) -> void:
	if _host_conn == null:
		return
	if not _host_conn["handshaked"]:
		# 房主的回握手（告知其名字，uid 恒为 1）。
		var rest = _try_handshake(_host_conn, chunk)
		if rest == null:
			return # 数据不全，继续等
		_host_conn["handshaked"] = true
		# player_meta_joined 延迟到 announce_ready()（见 _on_host_dial_ok 注释）。
		if rest.is_empty():
			return
		chunk = rest # 同一 chunk 中握手后的数据继续按数据帧处理
	_append_fragments(_host_conn, chunk)


func _on_stream_gone(conn: Dictionary) -> void:
	var uid := _uid_of_conn(conn)
	if uid > 0:
		_peers.erase(uid)
		_pending_handshakes.erase(conn["stream"].stream_id)
		peer_disconnected.emit(uid)
		player_meta_left.emit(uid)


func _on_host_gone() -> void:
	if _status != CONNECTION_CONNECTED:
		return
	_peers.clear()
	_host_conn = null
	_status = CONNECTION_DISCONNECTED
	server_disconnected.emit()


func _uid_of_conn(conn: Dictionary) -> int:
	for uid in _peers:
		if _peers[uid] == conn:
			return uid
	return -1


# ---- 握手与分帧 ----

func _send_handshake(st, uid: int, player_name: String) -> void:
	var body := JSON.stringify({"uid": uid, "name": player_name}).to_utf8_buffer()
	var buf := PackedByteArray()
	buf.resize(4 + body.size())
	buf.encode_u32(0, body.size())
	for i in body.size():
		buf[4 + i] = body[i]
	st.send_data(buf)


## 尝试从「握手缓冲 + 新数据」中解析握手帧。
## 返回握手后同一 chunk 中的剩余字节（可为空数组）；数据不全返回 null 继续等。
## 解析失败会 reset 流。对方 uid 存入 conn["pending_uid"]。
func _try_handshake(conn: Dictionary, chunk: PackedByteArray):
	var acc: PackedByteArray = conn["hbuf"] if conn.has("hbuf") else PackedByteArray()
	acc.append_array(chunk)
	if acc.size() < 4:
		conn["hbuf"] = acc
		return null
	var length := acc.decode_u32(0)
	if length > MAX_PACKET:
		conn["stream"].reset()
		return null
	if acc.size() < 4 + length:
		conn["hbuf"] = acc
		return null
	var body := acc.slice(4, 4 + length)
	var rest := acc.slice(4 + length)
	conn.erase("hbuf")
	var meta = JSON.parse_string(body.get_string_from_utf8())
	if meta is Dictionary and meta.has("uid"):
		conn["pending_name"] = str(meta.get("name", ""))
		conn["pending_uid"] = int(meta["uid"])
		return rest
	conn["stream"].reset()
	return null


## 追加数据并按 [len:4][packet] 切包入队。
func _append_fragments(conn: Dictionary, chunk: PackedByteArray) -> void:
	# 来源 uid：guest 端一律来自房主（1）；server 端查连接表。
	var from_uid := MultiplayerPeer.TARGET_PEER_SERVER
	if _is_host:
		from_uid = _uid_of_conn(conn)
	var buf: PackedByteArray = conn["rbuf"]
	buf.append_array(chunk)
	conn["rbuf"] = buf
	while buf.size() >= 4:
		var length := buf.decode_u32(0)
		if length > MAX_PACKET:
			conn["stream"].reset()
			return
		if buf.size() < 4 + length:
			break
		_incoming.append({"data": buf.slice(4, 4 + length), "peer": from_uid})
		# slice 返回新数组，原地收缩用剩余部分覆盖。
		var rest := buf.slice(4 + length)
		conn["rbuf"] = rest
		buf = rest


# ---- MultiplayerPeer 虚方法（MultiplayerPeerExtension GDScript 接口）----
# 指针版 _get_packet/_put_packet GDScript 无法实现，按官方文档使用
# _get_packet_script/_put_packet_script。

func _poll() -> void:
	pass # 切包在数据到达回调中完成；此处只保留状态语义


func _get_available_packet_count() -> int:
	return _incoming.size()


func _get_packet_script() -> PackedByteArray:
	if _incoming.is_empty():
		return PackedByteArray()
	var item: Dictionary = _incoming.pop_front()
	_last_packet_peer = int(item["peer"])
	return item["data"]


func _get_packet_peer() -> int:
	# 引擎在出队前先调用本方法取「队头包」的来源（SceneMultiplayer.poll
	# 先 get_packet_peer 再 get_packet），因此必须返回队头而非上一个包。
	if _incoming.is_empty():
		return _last_packet_peer
	return int(_incoming[0]["peer"])


func _put_packet_script(packet: PackedByteArray) -> Error:
	if packet.size() > MAX_PACKET:
		return ERR_OUT_OF_MEMORY
	var buf := PackedByteArray()
	buf.resize(4 + packet.size())
	buf.encode_u32(0, packet.size())
	for i in packet.size():
		buf[4 + i] = packet[i]

	if _is_host:
		# server：按 target 路由到指定 guest，0 = 广播全部。
		if _target_peer == 0:
			for uid in _peers:
				var err: Error = _peers[uid]["stream"].send_data(buf)
				if err != OK:
					return err
		else:
			var p = _peers.get(_target_peer)
			if p == null:
				return ERR_INVALID_PARAMETER
			return p["stream"].send_data(buf)
	else:
		# client：只有到房主的一条流。
		if _host_conn == null:
			return ERR_CONNECTION_ERROR
		return _host_conn["stream"].send_data(buf)
	return OK


func _get_connection_status() -> int:
	return _status


func _get_unique_id() -> int:
	return _my_uid


func _is_server() -> bool:
	return _is_host


func _is_server_relay_supported() -> bool:
	return true


func _is_refusing_new_connections() -> bool:
	return _refuse_new_connections


func _set_refuse_new_connections(enable: bool) -> void:
	_refuse_new_connections = enable


func _get_packet_mode() -> int:
	return TRANSFER_MODE_RELIABLE


func _get_packet_channel() -> int:
	return 0


func _get_transfer_mode() -> int:
	return TRANSFER_MODE_RELIABLE


func _get_transfer_channel() -> int:
	return 0


func _get_max_packet_size() -> int:
	return MAX_PACKET


func _set_target_peer(peer: int) -> void:
	_target_peer = peer


func _set_transfer_mode(_mode: int) -> void:
	pass # 桥接流恒为可靠有序


func _set_transfer_channel(_channel: int) -> void:
	pass # 频道不支持，恒为 0


## server 踢人：强制断开指定玩家。
func _disconnect_peer(peer: int, _force: bool) -> void:
	var p = _peers.get(peer)
	if p != null:
		_peers[peer]["stream"].reset()


## 关闭整个 peer。
func _close() -> void:
	close_room()
