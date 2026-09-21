@tool
extends EditorPlugin

## Lanet P2P 插件入口：注册 Lanet 自动加载单例。
##
## 启用后游戏脚本可通过 Lanet（即 LanetManager 类实例）访问高层 API。

const MANAGER_NAME := "Lanet"
const MANAGER_PATH := "res://addons/lanet/lanet_manager.gd"


func _enter_tree() -> void:
	if not ProjectSettings.has_setting("autoload/" + MANAGER_NAME):
		add_autoload_singleton(MANAGER_NAME, MANAGER_PATH)


func _exit_tree() -> void:
	# 只在插件被禁用时移除单例；保留用户工程设置由 Godot 处理。
	if ProjectSettings.has_setting("autoload/" + MANAGER_NAME):
		var path: String = ProjectSettings.get_setting("autoload/" + MANAGER_NAME)
		if path == MANAGER_PATH:
			remove_autoload_singleton(MANAGER_NAME)
