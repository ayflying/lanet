"""隔离验收必须不接触生产卷，失败也回收临时容器。"""
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from lanet_ops import Portainer


class IsolationTests(unittest.TestCase):
    def test_empty_directory_does_not_create_container(self):
        api = object.__new__(Portainer)
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(api, 'node', return_value={'Id': 'production'}), patch.object(api, 'request', return_value={'Image': 'sha256:test'}) as request:
                with self.assertRaisesRegex(RuntimeError, '没有 Linux'):
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
        with tempfile.TemporaryDirectory() as directory:
            (Path(directory) / 'sdk.test').write_bytes(b'test')
            with patch.object(api, 'node', return_value={'Id': 'production'}), patch.object(api, 'request', side_effect=request):
                with self.assertRaisesRegex(RuntimeError, '模拟启动失败'):
                    api.isolated_tests(directory)
        self.assertEqual(calls[-1][0], 'DELETE')


if __name__ == '__main__':
    unittest.main()
