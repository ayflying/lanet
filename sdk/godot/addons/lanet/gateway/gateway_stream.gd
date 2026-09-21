class_name LanetGatewayStream
extends RefCounted

## 一条经网关桥接的流。
##
## 数据面：send_data() → 网关 → 对端；对端数据经 data_received 信号送达。
## 生命周期：
##   - close_write()：半关闭写端——对端触发 eof 信号；本端此后不能再写，
##     但仍可继续读（对端发来的数据正常送达）。
##   - 对端半关闭 → eof 信号（本端仍可发送、仍可接收对端剩余数据）。
##   - reset() / 对端中止 → reset_received + closed 信号，双向立即失效。

signal data_received(chunk: PackedByteArray)
signal eof                      # 对端已半关闭写端（本端仍可发送）
signal reset_received           # 流被强制中止（双向失效）
signal closed                   # 流终结（本地关闭或对端终止）
signal dial_ok                  # 本端主动开流成功（仅出向流触发）
signal dial_failed(reason: String) # 本端主动开流失败（仅出向流触发）

var stream_id: int = 0
var protocol: String = ""
var remote_peer: String = ""    # 对端玩家名（入向流/桥接流有效）
var is_inbound: bool = false    # 是否对端主动开来的流

var _client: Node = null        # LanetGatewayClient（避免循环类型引用用 Node）
var _write_closed := false
var _finished := false


func _init(client: Node, id: int) -> void:
	_client = client
	stream_id = id


## 发送二进制数据。返回 OK 或错误码。
func send_data(chunk: PackedByteArray) -> Error:
	if _write_closed or _finished:
		return ERR_CONNECTION_ERROR
	return _client._send_data(stream_id, chunk)


## 发送 UTF-8 文本便捷方法。
func send_text(text: String) -> Error:
	return send_data(text.to_utf8_buffer())


## 半关闭写端：对端收到 eof；本端仍可继续接收对端数据。
func close_write() -> void:
	if _write_closed or _finished:
		return
	_write_closed = true
	_client._send_close(stream_id)


## 强制中止流（双向立即失效）。
func reset() -> void:
	if _finished:
		return
	_finished = true
	_client._send_reset(stream_id)
	closed.emit()


## 本端是否已半关闭写端。
func is_write_closed() -> bool:
	return _write_closed


# ---- 以下由 LanetGatewayClient 内部调用 ----

func _deliver(chunk: PackedByteArray) -> void:
	if not _finished:
		data_received.emit(chunk)


func _mark_dial_ok() -> void:
	dial_ok.emit()


func _mark_dial_failed(reason: String) -> void:
	if not _finished:
		_finished = true
		dial_failed.emit(reason)
		closed.emit()


func _mark_eof() -> void:
	if not _finished:
		eof.emit()


func _mark_reset() -> void:
	if not _finished:
		_finished = true
		reset_received.emit()
		closed.emit()


func _mark_local_finished() -> void:
	_finished = true
	closed.emit()
