extends Node2D

## Lanet 双人 P2P 示例：一人创建房间、另一人加入，双方方块实时同步。
##
## 前置：把 sdk/godot/addons/lanet 整个目录拷进本工程（res://addons/lanet），
## 并在 project.godot 启用插件（或手动 autoload LanetManager）。
##
## 玩法：方向键移动自己的方块；对方方块位置经 @rpc 同步。

const BOX_SIZE := Vector2(48, 48)
const SYNC_INTERVAL := 0.1

var _url_edit: LineEdit
var _invite_edit: LineEdit
var _name_edit: LineEdit
var _host_edit: LineEdit
var _log: RichTextLabel

var _own_box: ColorRect
var _remote_box: ColorRect
var _sync_accum := 0.0
var _remote_name := "?"


func _ready() -> void:
	_build_ui()
	_build_boxes()
	_wire_manager()


# ---- UI ----

func _build_ui() -> void:
	var canvas := CanvasLayer.new()
	add_child(canvas)
	var panel := PanelContainer.new()
	panel.set_anchors_preset(Control.PRESET_TOP_LEFT)
	panel.position = Vector2(8, 8)
	canvas.add_child(panel)
	var box := VBoxContainer.new()
	panel.add_child(box)

	_url_edit = _row(box, "网关地址", "ws://127.0.0.1:8700/gateway")
	_invite_edit = _row(box, "邀请码", "")
	_name_edit = _row(box, "你的名字", "player%d" % (randi() % 900 + 100))
	_host_edit = _row(box, "房主名字(加入时)", "")

	var buttons := HBoxContainer.new()
	box.add_child(buttons)
	var host_btn := Button.new()
	host_btn.text = "创建房间"
	host_btn.pressed.connect(_on_host_pressed)
	buttons.add_child(host_btn)
	var join_btn := Button.new()
	join_btn.text = "加入房间"
	join_btn.pressed.connect(_on_join_pressed)
	buttons.add_child(join_btn)

	_log = RichTextLabel.new()
	_log.custom_minimum_size = Vector2(420, 120)
	_log.bbcode_enabled = true
	box.add_child(_log)


func _row(parent: Container, label: String, default_text: String) -> LineEdit:
	var h := HBoxContainer.new()
	parent.add_child(h)
	var l := Label.new()
	l.text = label
	l.custom_minimum_size = Vector2(130, 0)
	h.add_child(l)
	var e := LineEdit.new()
	e.text = default_text
	e.custom_minimum_size = Vector2(280, 0)
	h.add_child(e)
	return e


func _log_line(text: String) -> void:
	_log.append_text(text + "\n")


# ---- 方块 ----

func _build_boxes() -> void:
	_own_box = ColorRect.new()
	_own_box.color = Color(0.30, 0.62, 1.00)
	_own_box.size = BOX_SIZE
	_own_box.position = Vector2(120, 300)
	add_child(_own_box)

	_remote_box = ColorRect.new()
	_remote_box.color = Color(1.00, 0.55, 0.35)
	_remote_box.size = BOX_SIZE
	_remote_box.position = Vector2(320, 300)
	add_child(_remote_box)


func _process(delta: float) -> void:
	# 自己的方块：方向键移动。
	var dir := Input.get_vector("ui_left", "ui_right", "ui_up", "ui_down")
	_own_box.position += dir * 240.0 * delta
	# 定期把位置广播给房间里所有人（@rpc）。
	if Lanet.peer != null:
		_sync_accum += delta
		if _sync_accum >= SYNC_INTERVAL:
			_sync_accum = 0.0
			sync_position.rpc(_own_box.position)


@rpc("any_peer", "call_remote", "reliable")
func sync_position(pos: Vector2) -> void:
	_remote_box.position = pos


# ---- 房间 ----

func _wire_manager() -> void:
	Lanet.room_joined.connect(func(info: Dictionary) -> void:
		_log_line("入房成功: %s" % str(info))
		# 入房时 SDK 已自动挂接 MultiplayerAPI；此处显式调用为幂等校验。
		if Lanet.make_multiplayer_peer() == null:
			_log_line("[color=red]MultiplayerPeer 初始化失败[/color]"))
	Lanet.room_error.connect(func(reason: String) -> void:
		_log_line("[color=red]房间错误: %s[/color]" % reason))
	Lanet.room_left.connect(func() -> void:
		_log_line("已离开房间"))
	Lanet.player_joined.connect(func(_uid: int, name: String) -> void:
		_remote_name = name
		_log_line("玩家加入: %s" % name))
	Lanet.player_left.connect(func(_uid: int) -> void:
		_log_line("玩家离开"))
	Lanet.data_received.connect(func(msg: Dictionary) -> void:
		_log_line("[消息] %s: %s" % [str(msg.get("from")), str(msg.get("data"))]))


func _on_host_pressed() -> void:
	_log_line("正在创建房间…")
	Lanet.create_room(_url_edit.text.strip_edges(),
		_invite_edit.text.strip_edges(), _name_edit.text.strip_edges())


func _on_join_pressed() -> void:
	_log_line("正在加入房间…")
	Lanet.join_room(_url_edit.text.strip_edges(),
		_invite_edit.text.strip_edges(), _name_edit.text.strip_edges(),
		_host_edit.text.strip_edges())


# 演示 JSON 消息（聊天）：按回车发送。
func _unhandled_key_input(event: InputEvent) -> void:
	if event is InputEventKey and event.pressed and event.keycode == KEY_ENTER:
		if Lanet.peer != null:
			Lanet.broadcast({"text": "hello from %s" % Lanet.my_name})
