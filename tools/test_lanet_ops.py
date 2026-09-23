"""lanet_ops 单元测试：隔离验收不接触生产，传输层/节点定位/资源解析均可离线验证。"""
import io
import json
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

import urllib.error
import urllib.request

from lanet_ops import Portainer, PortainerError, _http_get_json, _redact, path_of, watch_github


class FakeResp:
    """模拟 opener.open 返回的响应：按传入的分块列表依次返回，耗尽后返回空。"""
    def __init__(self, chunks):
        self._chunks = list(chunks)

    def read(self, amt=None):
        if self._chunks:
            return self._chunks.pop(0)
        return b''

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


class FakeOpener:
    """确定性的假 opener，避免 MagicMock 在流式读取时的歧义。"""
    def __init__(self, resp):
        self.resp = resp

    def open(self, req, timeout=None):
        return self.resp


def _http_error(code, body=b'err'):
    return urllib.error.HTTPError('http://x', code, 'x', {}, io.BytesIO(body))


def _build_api():
    api = Portainer.__new__(Portainer)
    api.base = 'https://example.test'
    api.prefix = ''
    api.key = 'super-secret-key'
    api.timeout = 10
    api.endpoint = 10
    api._direct = MagicMock()
    api._proxy = MagicMock()
    return api


class TransportTests(unittest.TestCase):
    def test_direct_success_no_proxy(self):
        api = _build_api()
        api._direct.open.return_value = FakeResp([json.dumps({'ok': 1}).encode()])
        self.assertEqual(api.request('GET', '/x'), {'ok': 1})
        self.assertEqual(api._direct.open.call_count, 1)
        self.assertEqual(api._proxy.open.call_count, 0)

    def test_fallback_to_proxy_on_transport_error(self):
        api = _build_api()
        api._direct.open.side_effect = urllib.error.URLError('conn refused')
        api._proxy.open.return_value = FakeResp([json.dumps({'ok': 2}).encode()])
        self.assertEqual(api.request('GET', '/x'), {'ok': 2})
        self.assertEqual(api._direct.open.call_count, 1)
        self.assertEqual(api._proxy.open.call_count, 1)

    def test_both_fail_raises_portainer_error(self):
        api = _build_api()
        api._direct.open.side_effect = urllib.error.URLError('no route')
        api._proxy.open.side_effect = socket_timeout()
        with self.assertRaises(PortainerError):
            api.request('GET', '/x')

    def test_5xx_retries_proxy_then_succeeds(self):
        api = _build_api()
        api._direct.open.side_effect = _http_error(503, b'busy')
        api._proxy.open.return_value = FakeResp([json.dumps({'ok': 3}).encode()])
        self.assertEqual(api.request('GET', '/x'), {'ok': 3})

    def test_4xx_raises_without_proxy_retry(self):
        api = _build_api()
        api._direct.open.side_effect = _http_error(401, b'unauthorized')
        with self.assertRaises(PortainerError):
            api.request('GET', '/x')
        self.assertEqual(api._proxy.open.call_count, 0)

    def test_all_requests_carry_user_agent(self):
        api = _build_api()
        api._direct.open.return_value = FakeResp([b'{}'])
        api.request('GET', '/x')
        req = api._direct.open.call_args[0][0]
        hdrs = {k.lower(): v for k, v in req.header_items()}
        self.assertEqual(hdrs.get('user-agent'), 'lanet-ops/1.0')
        self.assertEqual(hdrs.get('x-api-key'), 'super-secret-key')

    def test_empty_body_post_sets_content_length_zero(self):
        api = _build_api()
        api._direct.open.return_value = FakeResp([b'{}'])
        api.request('POST', '/x')  # data=None
        req = api._direct.open.call_args[0][0]
        hdrs = {k.lower(): v for k, v in req.header_items()}
        self.assertEqual(hdrs.get('content-length'), '0')

    def test_credential_never_in_error_output(self):
        api = _build_api()
        api._direct.open.side_effect = _http_error(500, b'leak')
        api._proxy.open.side_effect = _http_error(500, b'leak')
        with self.assertRaises(PortainerError) as ctx:
            api.request('GET', '/x')
        self.assertNotIn('super-secret-key', str(ctx.exception))

    def test_redact_strips_url_credentials(self):
        self.assertEqual(_redact('http://user:pass@host/api'), 'http://host/api')
        self.assertNotIn('user:pass@', _redact('https://u:p@h.x/api?q=1'))

    def test_download_streams_to_file(self):
        api = _build_api()
        payload = b'x' * 5000
        # 分块返回，验证流式拼接且最终正常结束。
        api._direct = FakeOpener(FakeResp([payload[:2500], payload[2500:]]))
        api._proxy = FakeOpener(FakeResp([b'']))
        import tempfile
        with tempfile.NamedTemporaryFile(delete=False) as tf:
            tmp = tf.name
        try:
            api._download('/archive', tmp)
            with open(tmp, 'rb') as fh:
                self.assertEqual(fh.read(), payload)
        finally:
            Path(tmp).unlink()


def socket_timeout():
    import socket
    return socket.timeout('timed out')


class NodeSelectionTests(unittest.TestCase):
    def _api_with_containers(self, containers):
        api = _build_api()
        api.request = MagicMock(return_value=containers)
        return api

    def test_select_by_exact_name(self):
        api = self._api_with_containers([
            {'Id': 'aaa', 'Names': ['/lanet-node'], 'Image': 'img'},
            {'Id': 'bbb', 'Names': ['/other'], 'Image': 'img'},
        ])
        self.assertEqual(api.node()['Id'], 'aaa')

    def test_fallback_to_compose_label(self):
        api = self._api_with_containers([
            {'Id': 'ccc', 'Names': ['/lanet-node-1'],
             'Labels': {'com.docker.compose.project': 'lanet', 'com.docker.compose.service': 'node'}},
        ])
        self.assertEqual(api.node()['Id'], 'ccc')

    def test_multiple_matches_raises(self):
        api = self._api_with_containers([
            {'Id': 'a', 'Names': ['/lanet-node']},
            {'Id': 'b', 'Names': ['/lanet-node']},
        ])
        with self.assertRaises(PortainerError):
            api.node()

    def test_none_found_raises(self):
        api = self._api_with_containers([])
        with self.assertRaises(PortainerError):
            api.node()


class StatsParseTests(unittest.TestCase):
    def _stats(self, cpu_total, sys_total, percpu):
        return {
            'cpu_stats': {'cpu_usage': {'total_usage': cpu_total, 'percpu_usage': percpu},
                          'system_cpu_usage': sys_total, 'online_cpus': len(percpu)},
            'memory_stats': {'usage': 200, 'limit': 1000},
            'networks': {'eth0': {'rx_bytes': 10, 'tx_bytes': 20}},
            'blkio_stats': {'io_service_bytes_recursive': [{'op': 'Read', 'value': 5}, {'op': 'Write', 'value': 7}]},
            'pids': 42,
        }

    def test_first_sample_has_no_cpu_percent(self):
        api = Portainer.__new__(Portainer)
        s = api._parse_stats(self._stats(100, 1000, [1, 1]), None)
        self.assertIsNone(s['cpu_percent'])

    def test_cpu_percent_computed_between_samples(self):
        api = Portainer.__new__(Portainer)
        prev = self._stats(100, 1000, [1, 1])
        cur = self._stats(200, 1100, [1, 1])  # cpu_delta=100, sys_delta=100, online=2 => 200%
        s = api._parse_stats(cur, prev)
        self.assertEqual(s['cpu_percent'], 200.0)
        self.assertEqual(s['memory_percent'], 20.0)
        self.assertEqual(s['network_rx_bytes'], 10)
        self.assertEqual(s['network_tx_bytes'], 20)
        self.assertEqual(s['block_io_bytes'], 12)
        self.assertEqual(s['pids'], 42)


class IsolationTests(unittest.TestCase):
    """沿用原有隔离验收断言：不挂生产卷、失败也回收临时容器。"""
    def test_empty_directory_does_not_create_container(self):
        api = object.__new__(Portainer)
        with patch.object(api, 'node', return_value={'Id': 'production'}), \
                patch.object(api, 'request', return_value={'Image': 'sha256:test'}) as request:
            import tempfile
            with tempfile.TemporaryDirectory() as directory:
                with self.assertRaisesRegex(PortainerError, '没有 Linux'):
                    api.isolated_tests(directory)
                self.assertEqual(request.call_count, 1)

    def test_failed_start_cleans_only_test_container(self):
        api = object.__new__(Portainer)
        calls = []
        def request(method, path, data=None, raw=False):
            calls.append((method, path, data))
            if path == '/containers/production/json':
                return {'Image': 'sha256:pinned'}
            if path.startswith('/containers/create'):
                self.assertEqual(data['Image'], 'sha256:pinned')
                self.assertNotIn('Binds', data['HostConfig'])
                self.assertNotIn('VolumesFrom', data['HostConfig'])
                self.assertEqual(data['HostConfig']['NetworkMode'], 'none')
                return {'Id': 'isolated'}
            if '/restart' in path:
                raise RuntimeError('模拟启动失败')
            if method == 'DELETE':
                self.assertEqual(path, '/containers/isolated?force=true&v=true')
                return None
            self.fail(f'未预期调用: {method} {path}')
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            (Path(directory) / 'sdk.test').write_bytes(b'test')
            with patch.object(api, 'node', return_value={'Id': 'production'}), \
                    patch.object(api, 'request', side_effect=request):
                with self.assertRaisesRegex(RuntimeError, '模拟启动失败'):
                    api.isolated_tests(directory)
        self.assertEqual(calls[-1][0], 'DELETE')


class CliDispatchTests(unittest.TestCase):
    def test_status_dispatch(self):
        api = _build_api()
        api.status = MagicMock(return_value={'id': 'x', 'state': 'running'})
        with patch('lanet_ops.Portainer', return_value=api), \
                patch('sys.stdout') as out:
            from lanet_ops import main
            rc = main(['status'])
        self.assertEqual(rc, 0)
        api.status.assert_called_once()

    def test_watch_github_forwards_sha(self):
        from lanet_ops import main
        sha = 'a' * 40
        with patch('lanet_ops.watch_github', return_value=0) as wg:
            rc = main(['watch-github', '--sha', sha, '--once'])
        self.assertEqual(rc, 0)
        wg.assert_called_once_with(sha, timeout=1800, once=True)


class GithubWatchTests(unittest.TestCase):
    SHA = 'a' * 40

    def _run(self, status='completed', conclusion='success', name='release'):
        return {'id': 42, 'head_sha': self.SHA, 'name': name,
                'status': status, 'conclusion': conclusion}

    def test_both_workflows_succeed_once(self):
        runs = {'workflow_runs': [self._run(), self._run(name='docker')]}
        with patch('lanet_ops._http_get_json', return_value=runs):
            self.assertEqual(watch_github(self.SHA, once=True), 0)

    def test_only_one_or_pending_is_not_success(self):
        with patch('lanet_ops._http_get_json', return_value={'workflow_runs': [self._run()]}):
            self.assertEqual(watch_github(self.SHA, once=True), 3)
        runs = {'workflow_runs': [self._run(), self._run('in_progress', None, 'docker')]}
        with patch('lanet_ops._http_get_json', return_value=runs):
            self.assertEqual(watch_github(self.SHA, once=True), 3)

    def test_failure_exits_without_waiting(self):
        runs = {'workflow_runs': [self._run('completed', 'failure'), self._run(name='docker')]}
        with patch('lanet_ops._http_get_json', return_value=runs):
            self.assertEqual(watch_github(self.SHA, once=True), 1)

    def test_other_commit_cannot_make_result_green(self):
        other = dict(self._run(name='docker'), head_sha='b' * 40)
        with patch('lanet_ops._http_get_json', return_value={'workflow_runs': [self._run(), other]}):
            self.assertEqual(watch_github(self.SHA, once=True), 3)

    def test_network_errors_exit_after_three(self):
        with patch('lanet_ops._http_get_json', side_effect=PortainerError('临时失败')) as get, \
                patch('lanet_ops.time.sleep'):
            self.assertEqual(watch_github(self.SHA, timeout=60), 2)
        self.assertEqual(get.call_count, 3)

    def test_invalid_sha_rejected_before_request(self):
        with patch('lanet_ops._http_get_json') as get:
            with self.assertRaisesRegex(PortainerError, '40 位'):
                watch_github('abc123', once=True)
        get.assert_not_called()

    def test_github_request_uses_token_and_direct_first(self):
        direct = MagicMock()
        proxy = MagicMock()
        direct.open.return_value = FakeResp([b'{}'])
        with patch('lanet_ops._make_opener', side_effect=[direct, proxy]), \
                patch.dict('lanet_ops.os.environ', {'GITHUB_TOKEN': 'test-secret'}, clear=True):
            self.assertEqual(_http_get_json('https://api.github.com/x'), {})
        req = direct.open.call_args.args[0]
        hdrs = {k.lower(): v for k, v in req.header_items()}
        self.assertEqual(hdrs['authorization'], 'Bearer test-secret')
        proxy.open.assert_not_called()

    def test_github_503_falls_back_to_proxy(self):
        direct = MagicMock()
        proxy = MagicMock()
        direct.open.side_effect = _http_error(503)
        proxy.open.return_value = FakeResp([b'{"ok":true}'])
        with patch('lanet_ops._make_opener', side_effect=[direct, proxy]), \
                patch.dict('lanet_ops.os.environ', {}, clear=True):
            self.assertEqual(_http_get_json('https://api.github.com/x'), {'ok': True})
        proxy.open.assert_called_once()

    def test_github_403_does_not_leak_token_or_retry(self):
        direct = MagicMock()
        proxy = MagicMock()
        direct.open.side_effect = _http_error(403, b'test-secret')
        with patch('lanet_ops._make_opener', side_effect=[direct, proxy]), \
                patch.dict('lanet_ops.os.environ', {'GITHUB_TOKEN': 'test-secret'}, clear=True):
            with self.assertRaises(PortainerError) as ctx:
                _http_get_json('https://api.github.com/x')
        self.assertNotIn('test-secret', str(ctx.exception))
        proxy.open.assert_not_called()


if __name__ == '__main__':
    unittest.main()
