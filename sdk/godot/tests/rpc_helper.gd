extends Node

## e2e 测试用的 RPC 助手节点：验证 @rpc 经 LanetMultiplayerPeer 的全链路。

signal ping_received(from_uid: int, value: int)
signal pong_received(from_uid: int, value: int)


@rpc("any_peer", "call_remote", "reliable")
func ping(value: int) -> void:
	ping_received.emit(multiplayer.get_remote_sender_id(), value)


@rpc("any_peer", "call_remote", "reliable")
func pong(value: int) -> void:
	pong_received.emit(multiplayer.get_remote_sender_id(), value)
