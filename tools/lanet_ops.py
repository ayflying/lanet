"""通过 Portainer 在生产节点同镜像的隔离容器中运行 Linux 测试。"""
import argparse
import io
import gzip
import json
import os
from pathlib import Path
import tarfile
import urllib.request
import urllib.error
import http.client
import urllib.parse
import uuid


class Portainer:
    def __init__(self, endpoint=10):
        self.base = os.environ.get('PORTAINER_URL', 'https://docker.luoe.cn').rstrip('/')
        self.key = os.environ['PORTAINER_API_KEY']
        self.prefix = f'/api/endpoints/{endpoint}/docker/v1.41'
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(self, method, path, data=None, raw=False):
        headers = {'X-API-Key': self.key, 'User-Agent': 'lanet-ops/1.0'}
        if isinstance(data, (dict, list)):
            data = json.dumps(data).encode()
            headers['Content-Type'] = 'application/json'
        elif data is not None:
            headers['Content-Type'] = 'application/x-tar'
        if method == 'POST' and data is None:
            # 明确发送无正文请求，避免反向代理将其转换为分块正文。
            target = urllib.parse.urlsplit(self.base)
            connection = http.client.HTTPSConnection(target.hostname, target.port, timeout=240)
            try:
                connection.request(method, target.path + self.prefix + path, headers={**headers, 'Content-Length': '0'})
                response = connection.getresponse()
                body = response.read()
                if response.status >= 400:
                    raise RuntimeError(f'{method} {path}: HTTP {response.status}: {body.decode(errors="replace")}')
                return body if raw else (json.loads(body) if body else None)
            finally:
                connection.close()
        req = urllib.request.Request(self.base + self.prefix + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(req, timeout=240) as response:
                body = response.read()
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode('utf-8', errors='replace')
            raise RuntimeError(f'{method} {path}: HTTP {exc.code}: {detail}') from None
        return body if raw else (json.loads(body) if body else None)

    def node(self):
        nodes = [c for c in self.request('GET', '/containers/json')
                 if c.get('Labels', {}).get('com.docker.compose.project') == 'lanet'
                 and c.get('Labels', {}).get('com.docker.compose.service') == 'node']
        if len(nodes) != 1:
            raise RuntimeError(f'必须唯一匹配运行中的 lanet/node，实际 {len(nodes)}')
        return nodes[0]

    def isolated_tests(self, directory):
        node = self.node()
        inspect = self.request('GET', '/containers/' + node['Id'] + '/json')
        image = inspect['Image']
        source = Path(directory).resolve()
        tests = sorted(source.glob('*.test'))
        if not tests:
            raise RuntimeError('没有 Linux 测试二进制，拒绝创建容器')
        cid = None
        try:
            spec = {'Image': image, 'Entrypoint': ['/bin/sh'], 'Cmd': ['-c', 'sleep 7200'],
                    'Labels': {'lanet.stability-test': 'true'},
                    'HostConfig': {'NetworkMode': 'none', 'Memory': 1073741824, 'PidsLimit': 256,
                                   'CapDrop': ['ALL'], 'SecurityOpt': ['no-new-privileges:true']}}
            cid = self.request('POST', '/containers/create?name=lanet-test-' + uuid.uuid4().hex[:12], spec)['Id']
            self.request('POST', '/containers/' + cid + '/restart?t=1')
            for test in tests:
                print(f'分片上传 {test.name}', flush=True)
                compressed = gzip.compress(test.read_bytes(), compresslevel=1)
                with io.BytesIO(compressed) as stream:
                    part = 0
                    while chunk := stream.read(1024 * 1024):
                        archive = io.BytesIO()
                        with tarfile.open(fileobj=archive, mode='w:gz') as tar:
                            info = tarfile.TarInfo(f'lanet-tests/{test.name}.part{part:04d}')
                            info.size = len(chunk)
                            info.mode = 0o600
                            tar.addfile(info, io.BytesIO(chunk))
                        for attempt in range(3):
                            try:
                                self.request('PUT', '/containers/' + cid + '/archive?path=/tmp', archive.getvalue())
                                break
                            except RuntimeError as exc:
                                if attempt == 2 or not any(code in str(exc) for code in ('HTTP 500', 'HTTP 502', 'HTTP 503', 'HTTP 504')):
                                    raise
                        part += 1
                # 文件名只来自固定构建产物，拒绝任何 shell 元字符。
                if any(ch not in 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-' for ch in test.name):
                    raise RuntimeError('非法测试文件名')
                command = f'cat /tmp/lanet-tests/{test.name}.part* | gzip -d > /tmp/lanet-tests/{test.name} && chmod 755 /tmp/lanet-tests/{test.name}'
                eid = self.request('POST', '/containers/' + cid + '/exec', {'AttachStdout': True, 'AttachStderr': True, 'Tty': True, 'Cmd': ['/bin/sh', '-c', command]})['Id']
                self.request('POST', '/exec/' + eid + '/start', {'Detach': False, 'Tty': True}, raw=True)
                state = self.request('GET', '/exec/' + eid + '/json')
                if state['Running'] or state['ExitCode'] != 0:
                    raise RuntimeError('测试文件分片合并失败')
            results = []
            for test in tests:
                eid = self.request('POST', '/containers/' + cid + '/exec', {
                    'AttachStdout': True, 'AttachStderr': True, 'Tty': True,
                    'Cmd': ['/tmp/lanet-tests/' + test.name, '-test.v', '-test.timeout=150s',
                            '-test.coverprofile=/tmp/lanet-tests/' + test.stem + '.cover']})['Id']
                output = self.request('POST', '/exec/' + eid + '/start', {'Detach': False, 'Tty': True}, raw=True)
                state = self.request('GET', '/exec/' + eid + '/json')
                (source / (test.stem + '.log')).write_bytes(output)
                if state['Running']:
                    raise RuntimeError('测试仍在运行，不能判定通过')
                results.append({'test': test.name, 'exit_code': state['ExitCode']})
            for test in tests:
                bundle = self.request('GET', '/containers/' + cid + '/archive?path=/tmp/lanet-tests/' + test.stem + '.cover', raw=True)
                with tarfile.open(fileobj=io.BytesIO(bundle)) as tar:
                    for member in tar.getmembers():
                        if member.isfile() and member.name.endswith('.cover'):
                            (source / Path(member.name).name).write_bytes(tar.extractfile(member).read())
            report = {'production_image': node['Image'], 'image_id': image, 'isolated': True, 'results': results}
            (source / 'results.json').write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
            print(json.dumps(report, ensure_ascii=False))
            return int(any(r['exit_code'] != 0 for r in results))
        finally:
            if cid:
                self.request('DELETE', '/containers/' + cid + '?force=true&v=true')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['status', 'isolated-tests'])
    parser.add_argument('--directory')
    args = parser.parse_args()
    api = Portainer()
    if args.command == 'status':
        node = api.node()
        print(json.dumps({'id': node['Id'], 'image': node['Image'], 'status': node['Status']}, ensure_ascii=False))
        return 0
    if not args.directory:
        parser.error('isolated-tests 需要 --directory')
    return api.isolated_tests(args.directory)


if __name__ == '__main__':
    raise SystemExit(main())
