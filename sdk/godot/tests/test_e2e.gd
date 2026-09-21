extends SceneTree

## Lanet Godot SDK 端到端测试（headless）。
##
## 由 Go 集成测试驱动：起真实网关（httptest + handleWS），本脚本以 3 个
## 进程分别运行（host / guestA / guestB），验证：
##   1. 网关鉴权 + 房间建立（_mgr.create_room / join_room）
##   2. 玩家加入事件（握手、uid 分配）
##   3. @rpc 全链路：guest→host、host→guest、guest→guest（经房主 relay）
##   4. JSON 消息（send_to / data_received）双向往返
##
## 通过标准：打印 E2E-PASS 并以退出码 0 结束；任何一步超时打印
## E2E-FAIL 并以退出码 1 结束。

const HELPER := preload("res://tests/rpc_helper.gd")
const MANAGER := preload("res://addons/lanet/lanet_manager.gd")

var _mgr = null   # LanetManager 实例（--script 模式下 autoload 全局名不可用，手动实例化）
var _ok := {}
var _flags := {}


func _initialize() -> void:
	_mgr = MANAGER.new()
	root.add_child(_mgr)
	var args := {}
	for raw in OS.get_cmdline_user_args():
		var kv := raw.trim_prefix("--").split("=")
		if kv.size() == 2:
			args[kv[0]] = kv[1]
	var mode := str(args.get("mode", ""))
	var url := str(args.get("url", ""))
	if mode == "" or url == "":
		_fail("缺少参数 mode/url")
		return
	match mode:
		"host":
			_run_host(url)
		"guestA":
			_run_guest(url, "ga")
		"guestB":
			_run_guest(url, "gb")
		_:
			_fail("未知 mode: " + mode)


# ---- 断言与等待 ----

func _flag(key: String) -> bool:
	return _flags.get(key, false)


func _mark(key: String) -> void:
	_flags[key] = true


func _wait_flag(key: String, timeout := 15.0) -> bool:
	var t := 0.0
	while t < timeout:
		if _flag(key):
			return true
		await create_timer(0.05).timeout
		t += 0.05
	return false


func _pass(what: String) -> void:
	print("E2E-PASS [%s] %s" % [OS.get_cmdline_user_args()[0], what])
	quit(0)


func _fail(what: String) -> void:
	print("E2E-FAIL %s" % what)
	quit(1)


# ---- host ----

func _run_host(url: String) -> void:
	_mgr.player_joined.connect(func(_uid: int, name: String) -> void:
		_mark("joined_" + name))
	_mgr.data_received.connect(func(msg: Dictionary) -> void:
		var data: Dictionary = msg.get("data", {})
		if str(data.get("hello", "")) == "world":
			_mark("chat_from_ga")
			_mgr.send_to(str(msg.get("from")), {"reply": true}))
	_mgr.room_error.connect(func(r: String) -> void:
		_fail("host room_error: " + r))
	_mgr.create_room(url, "", "host")
	if not await _wait_flag_by_signal(_mgr.room_joined):
		_fail("host 入房超时")
		return
	if _mgr.make_multiplayer_peer() == null:
		_fail("make_multiplayer_peer 返回空")
		return
	print("host uid=%d" % root.get_multiplayer().get_unique_id())

	var helper := HELPER.new()
	helper.name = "helper"
	root.add_child(helper)
	helper.ping_received.connect(func(from_uid: int, value: int) -> void:
		helper.pong.rpc_id(from_uid, value)
		_mark("ping_from_%d" % value))

	# 等两个 guest 握手完成。
	if not await _wait_flag("joined_ga") or not await _wait_flag("joined_gb"):
		_fail("host 等 guest 加入超时")
		return
	# 等 guestA 的 ping（ping_from_42）。
	if not await _wait_flag("ping_from_42"):
		_fail("host 未收到 guestA ping")
		return
	# 等聊天消息（guestA send_to host）。
	if not await _wait_flag("chat_from_ga"):
		_fail("host 未收到 guestA 消息")
		return
	_pass("host")


func _wait_flag_by_signal(sig: Signal) -> bool:
	sig.connect(func(_x) -> void: _mark("room_ok"))
	if not await _wait_flag("room_ok", 15.0):
		return false
	return true


# ---- guest ----

func _run_guest(url: String, my_name: String) -> void:
	_mgr.data_received.connect(func(msg: Dictionary) -> void:
		if msg.get("data", {}).get("reply", false):
			_mark("chat_reply"))
	_mgr.room_error.connect(func(r: String) -> void:
		_fail("%s room_error: %s" % [my_name, r]))

	var is_a := my_name == "ga"
	_mgr.join_room(url, "", my_name, "host")
	var ok := await _wait_guest_ready()
	if not ok:
		_fail("%s 入房超时" % my_name)
		return

	var helper := HELPER.new()
	helper.name = "helper"
	root.add_child(helper)
	var pong_seen := [false]
	var relay_seen := [false]
	helper.pong_received.connect(func(_from: int, value: int) -> void:
		if value == 42:
			pong_seen[0] = true
		if value == 778:
			relay_seen[0] = true) # gb 经房主 relay 回来的 pong
	helper.ping_received.connect(func(_from: int, value: int) -> void:
		if value == 777:
			relay_seen[0] = true # 收到 relay ping（gb 视角的验收点）
			helper.pong.rpc_id(helper.get_multiplayer().get_remote_sender_id(), 778))

	if is_a:
		# 1) guest→host 的 @rpc。
		helper.ping.rpc_id(1, 42)
		var t := 0.0
		while not pong_seen[0] and t < 15.0:
			await create_timer(0.05).timeout
			t += 0.05
		if not pong_seen[0]:
			_fail("ga 未收到 host pong")
			return
		# 2) guest→guest（经房主 relay）：发给 gb。先等名册同步出 gb。
		var gb_uid := -1
		var t2 := 0.0
		while gb_uid < 0 and t2 < 15.0:
			gb_uid = _uid_of("gb")
			if gb_uid < 0:
				await create_timer(0.05).timeout
				t2 += 0.05
		if gb_uid < 0:
			_fail("ga 查不到 gb 的 uid")
			return
		helper.ping.rpc_id(gb_uid, 777)
		t = 0.0
		while not relay_seen[0] and t < 15.0:
			await create_timer(0.05).timeout
			t += 0.05
		if not relay_seen[0]:
			_fail("ga 未收到 gb 的 relay 回包")
			return
		# 3) JSON 消息：send_to host，等回复。
		var err: int = _mgr.send_to("host", {"hello": "world"})
		if err != OK:
			_fail("ga send_to 失败: " + error_string(err))
			return
		t = 0.0
		while not _flag("chat_reply") and t < 15.0:
			await create_timer(0.05).timeout
			t += 0.05
		if not _flag("chat_reply"):
			_fail("ga 未收到 host 回复")
			return
	else:
		# gb：等 ga 的 relay ping，回 pong（在 ping_received 回调里）。
		var t := 0.0
		while not relay_seen[0] and t < 20.0:
			await create_timer(0.05).timeout
			t += 0.05
		if not relay_seen[0]:
			_fail("gb 未收到 ga 的 relay ping")
			return
	_pass(my_name)


func _wait_guest_ready() -> bool:
	# room_joined 且 multiplayer 已挂接。
	var t := 0.0
	while t < 15.0:
		if _mgr.peer != null and root.get_multiplayer().multiplayer_peer != null \
				and root.get_multiplayer().get_unique_id() >= 2:
			return true
		await create_timer(0.05).timeout
		t += 0.05
	return false


func _uid_of(player_name: String) -> int:
	return _mgr.uid_of(player_name)
