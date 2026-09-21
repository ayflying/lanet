extends SceneTree

## 帧协议编解码单测（headless）：
## 与 Go pkg/gatewayproto 同一向量，验证 GDScript 端字节级兼容。
## 通过打印 FRAME-PASS 并以退出码 0 结束。

const FRAME := preload("res://addons/lanet/gateway/gateway_frame.gd")


func _initialize() -> void:
	var failed := false

	# 向量 1：空载荷 Data 帧 [type=7][sid=1][len=0]。
	var f1 := FRAME.marshal(FRAME.TYPE_DATA, 1, PackedByteArray())
	var expect1 := PackedByteArray([0x07, 0, 0, 0, 1, 0, 0, 0, 0])
	if f1 != expect1:
		print("FAIL: 空载荷帧字节不一致: ", f1)
		failed = true

	# 向量 2：载荷 "hi" 的 Data 帧 [type=7][sid=2][len=2][68 69]。
	var f2 := FRAME.marshal(FRAME.TYPE_DATA, 2, "hi".to_utf8_buffer())
	var expect2 := PackedByteArray([0x07, 0, 0, 0, 2, 0, 0, 0, 2, 0x68, 0x69])
	if f2 != expect2:
		print("FAIL: 载荷帧字节不一致: ", f2)
		failed = true

	# 往返：编码 → 解码 → 字段一致。
	var f3 := FRAME.marshal(FRAME.TYPE_STREAM_OPEN, 0x01020304,
		JSON.stringify({"protocol": "/game/1.0.0", "remote_peer": "alice"}).to_utf8_buffer())
	var d3 := FRAME.unmarshal(f3)
	if d3.is_empty() or int(d3["type"]) != FRAME.TYPE_STREAM_OPEN \
			or int(d3["stream_id"]) != 0x01020304:
		print("FAIL: 流打开帧往返不一致: ", d3)
		failed = true
	var meta := FRAME.payload_json(d3)
	if str(meta.get("protocol")) != "/game/1.0.0" or str(meta.get("remote_peer")) != "alice":
		print("FAIL: JSON 载荷往返不一致: ", meta)
		failed = true

	# 截断与空输入拒绝。
	if not FRAME.unmarshal(PackedByteArray([0x07, 0, 0])).is_empty():
		print("FAIL: 截断帧应被拒绝")
		failed = true
	if not FRAME.unmarshal(PackedByteArray()).is_empty():
		print("FAIL: 空输入应被拒绝")
		failed = true

	# 大 streamID 边界（0xFFFFFFFF）。
	var f4 := FRAME.marshal(FRAME.TYPE_DATA, 0xFFFFFFFF, PackedByteArray())
	var d4 := FRAME.unmarshal(f4)
	if int(d4["stream_id"]) != 0xFFFFFFFF:
		print("FAIL: 大 streamID 往返不一致")
		failed = true

	if failed:
		quit(1)
	else:
		print("FRAME-PASS")
		quit(0)
