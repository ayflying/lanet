#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Lanet 公网节点运维验收工具（仅 tools/ 目录，不提交、不升 VERSION、不写记忆）。

子命令：
  status            查询 lanet/node 容器运行状态与镜像
  backup-stack     备份 /data 数据卷与容器配置到本地 tar.gz（升级前快照）
  exec             在 node 容器内执行命令，取回输出与退出码
  upload-tests     把 *.test 二进制分片上传到容器内临时目录
  watch-github     轮询 GitHub Actions，等待 docker/release 流水线结束
  resource-sample  采样 node 容器 CPU/内存/网络/磁盘 资源占用
  isolated-tests   在隔离容器里用同镜像跑 Linux 测试（验收用，隔离生产卷）

设计约束：
  * 所有 HTTP 请求统一带 User-Agent 头。
  * 直连（无代理）失败时自动改用环境代理（HTTP(S)_PROXY）重试。
  * 凭据（PORTAINER_API_KEY）仅来自环境变量，绝不出现在任何标准输出/错误中；
    出错信息会剔除 URL 内嵌的用户名密码。
"""

import argparse
import gzip
import io
import json
import os
import re
import socket
import sys
import tarfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path

UA = 'lanet-ops/1.0'
ALLOWED_FN = set('abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-')
RETRYABLE_HTTP = (500, 502, 503, 504)


class PortainerError(Exception):
    """运维操作层面的错误（不含任何凭据）。"""


def _redact(text):
    """剔除文本中可能出现的 URL 内嵌凭证，避免凭据落到日志/输出。"""
    # 去掉 https?://user:pass@ 中的 user:pass@
    text = re.sub(r'([a-zA-Z][a-zA-Z0-9+.\-]*://)[^@/\s]+@', r'\1', text)
    return text


def _make_opener(use_proxy):
    """构造 opener：use_proxy=False 强制直连（空代理），True 读取环境代理。"""
    if use_proxy:
        return urllib.request.build_opener()  # 默认 ProxyHandler 读取 HTTP(S)_PROXY
    return urllib.request.build_opener(urllib.request.ProxyHandler({}))


class Portainer:
    def __init__(self, endpoint=None, timeout=240):
        self.base = os.environ.get('PORTAINER_URL', 'https://docker.luoe.cn').rstrip('/')
        # 凭据仅来自环境变量，绝不打印。
        try:
            self.key = os.environ['PORTAINER_API_KEY']
        except KeyError:
            raise PortainerError('缺少环境变量 PORTAINER_API_KEY') from None
        if endpoint is None:
            endpoint = int(os.environ.get('PORTAINER_ENDPOINT', '10'))
        self.endpoint = endpoint
        self.prefix = f'/api/endpoints/{endpoint}/docker/v1.41'
        self.timeout = timeout
        self._direct = _make_opener(False)
        self._proxy = _make_opener(True)

    # ------------------------------------------------------------------ 传输层
    def request(self, method, path, data=None, raw=False, timeout=None):
        """统一的 Docker API 调用：直连优先，失败走代理重试；所有请求带 UA。"""
        url = self.base + self.prefix + path
        timeout = self.timeout if timeout is None else timeout
        headers = {'X-API-Key': self.key, 'User-Agent': UA}
        if method == 'POST' and data is None:
            # 空正文 POST 必须显式 Content-Length:0，避免反向代理改写为 chunked。
            body = b''
            headers['Content-Length'] = '0'
        else:
            body = self._encode(data, headers)
        return self._do(url, method, body, headers, raw, timeout)

    def _do(self, url, method, body, headers, raw, timeout):
        last_exc = None
        for opener in (self._direct, self._proxy):
            try:
                req = urllib.request.Request(url, data=body, headers=headers, method=method)
                with opener.open(req, timeout=timeout) as resp:
                    content = resp.read()
                if raw:
                    return content
                return json.loads(content) if content else None
            except urllib.error.HTTPError as exc:
                detail = exc.read().decode('utf-8', errors='replace')
                if exc.code in RETRYABLE_HTTP and opener is self._direct:
                    last_exc = exc
                    continue  # 网关临时错误，换代理重试
                raise PortainerError(_redact(f'{method} {path_of(url)}: HTTP {exc.code}: {detail}')) from None
            except (urllib.error.URLError, socket.timeout, OSError, ConnectionError) as exc:
                last_exc = exc
                continue
        raise PortainerError(_redact(f'{method} {path_of(url)}: 直连与代理均失败: {last_exc}'))

    def _download(self, path, dest, timeout=None):
        """把 Docker API 的响应体流式写入本地文件（用于大体积备份）。"""
        url = self.base + self.prefix + path
        timeout = self.timeout if timeout is None else timeout
        headers = {'X-API-Key': self.key, 'User-Agent': UA}
        last_exc = None
        for opener in (self._direct, self._proxy):
            try:
                req = urllib.request.Request(url, headers=headers, method='GET')
                with opener.open(req, timeout=timeout) as resp:
                    with open(dest, 'wb') as fh:
                        while True:
                            chunk = resp.read(1024 * 1024)
                            if not chunk:
                                break
                            fh.write(chunk)
                return
            except urllib.error.HTTPError as exc:
                if exc.code in RETRYABLE_HTTP and opener is self._direct:
                    last_exc = exc
                    continue
                detail = exc.read().decode('utf-8', errors='replace')
                raise PortainerError(_redact(f'GET {path}: HTTP {exc.code}: {detail}')) from None
            except (urllib.error.URLError, socket.timeout, OSError, ConnectionError) as exc:
                last_exc = exc
                continue
        raise PortainerError(_redact(f'GET {path}: 直连与代理均失败: {last_exc}'))

    @staticmethod
    def _encode(data, headers):
        if data is None:
            return None
        if isinstance(data, (bytes, bytearray)):
            return bytes(data)
        if isinstance(data, (dict, list)):
            headers.setdefault('Content-Type', 'application/json')
            return json.dumps(data).encode()
        raise TypeError('data 必须是 dict/list/bytes')

    # ------------------------------------------------------------------ 节点定位
    def node(self, name='lanet-node'):
        """定位唯一的 lanet/node 容器：优先精确容器名，回退 compose 标签。"""
        containers = self.request('GET', '/containers/json?all=1')
        # Docker 的 Names 带前导斜杠（如 /lanet-node），两侧都归一化后再比对。
        def names_of(c):
            return [n.lstrip('/') for n in (c.get('Names') or [])]
        by_name = [c for c in containers if name in names_of(c)]
        if len(by_name) == 1:
            return by_name[0]
        by_label = [c for c in containers
                    if c.get('Labels', {}).get('com.docker.compose.project') == 'lanet'
                    and c.get('Labels', {}).get('com.docker.compose.service') == 'node']
        if len(by_label) == 1:
            return by_label[0]
        if len(by_name) > 1:
            raise PortainerError(f'匹配到多个 {name} 容器：{len(by_name)}')
        if len(by_label) > 1:
            raise PortainerError(f'匹配到多个 lanet/node 容器：{len(by_label)}')
        raise PortainerError('未找到运行中的 lanet/node 容器')

    # ------------------------------------------------------------------ 子命令实现
    def status(self):
        node = self.node()
        inspect = self.request('GET', '/containers/' + node['Id'] + '/json')
        names = node.get('Names') or ['?']
        return {
            'id': node['Id'],
            'name': names[0].lstrip('/'),
            'image': node.get('Image'),
            'image_id': inspect.get('Image'),
            'state': inspect.get('State', {}).get('Status'),
            'running': inspect.get('State', {}).get('Running'),
            'created': inspect.get('Created'),
            'started_at': inspect.get('State', {}).get('StartedAt'),
            'network_mode': inspect.get('HostConfig', {}).get('NetworkMode'),
            'endpoint': self.endpoint,
        }

    def backup_stack(self, output_dir=None):
        """备份 node 容器 /data 数据卷与容器配置到本地带时间戳的目录。"""
        node = self.node()
        stamp = time.strftime('%Y%m%d-%H%M%S')
        out = Path(output_dir).resolve() if output_dir else Path(f'lanet-backup-{stamp}')
        out.mkdir(parents=True, exist_ok=True)
        # 1) 流式下载 /data 卷（含 node.key / lanet.json / state.json / lanet.db / lanet.log）
        data_archive = out / f'data-{stamp}.tar'
        self._download(f'/containers/{node["Id"]}/archive?path=/data', data_archive)
        # 2) 备份容器 inspect 元信息
        inspect = self.request('GET', '/containers/' + node['Id'] + '/json')
        (out / 'inspect.json').write_text(json.dumps(inspect, ensure_ascii=False, indent=2), encoding='utf-8')
        manifest = {
            'container': node['Id'],
            'container_name': (node.get('Names') or ['?'])[0].lstrip('/'),
            'image': node.get('Image'),
            'data_archive': data_archive.name,
            'created_at': stamp,
            'restore_hint': ('恢复：docker run --rm -v lanet-node-data:/data '
                            f'-v {out}:/backup busybox tar xf /backup/{data_archive.name} -C /data'),
        }
        (out / 'manifest.json').write_text(json.dumps(manifest, ensure_ascii=False, indent=2), encoding='utf-8')
        return {'output': str(out), 'data_archive': data_archive.name,
                'container': node['Id'], 'image': node.get('Image')}

    def exec_command(self, cmd, timeout=None):
        """在 node 容器内执行命令（/bin/sh -c），返回退出码与输出。"""
        node = self.node()
        spec = {'AttachStdout': True, 'AttachStderr': True, 'Tty': True,
                'Cmd': ['/bin/sh', '-c', cmd]}
        eid = self.request('POST', f"/containers/{node['Id']}/exec", spec)['Id']
        # Tty=True 时返回原始流（无 8 字节多路复用头），便于直接回显。
        output = self.request('POST', f"/exec/{eid}/start", {'Detach': False, 'Tty': True},
                              raw=True, timeout=timeout)
        state = self.request('GET', f"/exec/{eid}/json")
        return {'exit_code': state.get('ExitCode'),
                'output': output.decode('utf-8', errors='replace')}

    def upload_tests(self, directory, remote_dir='/tmp/lanet-tests'):
        """把 *.test 二进制分片压缩上传到容器内，再合并还原可执行文件。"""
        node = self.node()
        source = Path(directory).resolve()
        tests = sorted(source.glob('*.test'))
        if not tests:
            raise PortainerError('没有匹配 *.test 的二进制，拒绝上传')
        uploaded = []
        for test in tests:
            if any(ch not in ALLOWED_FN for ch in test.name):
                raise PortainerError('非法测试文件名: ' + test.name)
            self._upload_file(node['Id'], test, remote_dir)
            uploaded.append(test.name)
        return {'uploaded': uploaded, 'remote_dir': remote_dir}

    def _upload_file(self, cid, local_path, remote_dir):
        compressed = gzip.compress(local_path.read_bytes(), compresslevel=1)
        with io.BytesIO(compressed) as stream:
            part = 0
            while chunk := stream.read(1024 * 1024):
                archive = io.BytesIO()
                with tarfile.open(fileobj=archive, mode='w:gz') as tar:
                    info = tarfile.TarInfo(f'{local_path.name}.part{part:04d}')
                    info.size = len(chunk)
                    info.mode = 0o600
                    tar.addfile(info, io.BytesIO(chunk))
                data = archive.getvalue()
                for attempt in range(3):
                    try:
                        self.request('PUT', f'/containers/{cid}/archive?path={remote_dir}', data, raw=True)
                        break
                    except PortainerError as exc:
                        if attempt == 2 or 'HTTP 5' not in str(exc):
                            raise
                part += 1
        # 合并分片（文件名已校验，remote_dir 由运维指定）。
        command = (f'mkdir -p {remote_dir} && '
                   f'cat {remote_dir}/{local_path.name}.part* | gzip -d > {remote_dir}/{local_path.name} '
                   f'&& chmod 755 {remote_dir}/{local_path.name}')
        result = self.exec_command(command)
        if result['exit_code'] != 0:
            raise PortainerError('测试文件分片合并失败: ' + result['output'])

    def resource_sample(self, count=1, interval=1.0):
        """采样 node 容器资源占用。count>1 时以相邻两次差值计算 CPU 百分比。"""
        node = self.node()
        samples = []
        prev = None
        for i in range(max(1, count)):
            stats = self.request('GET', f"/containers/{node['Id']}/stats?stream=false")
            samples.append(self._parse_stats(stats, prev))
            prev = stats
            if i < count - 1:
                time.sleep(interval)
        return samples

    @staticmethod
    def _parse_stats(stats, prev):
        cs = stats.get('cpu_stats', {})
        prev_cs = (prev or stats).get('cpu_stats', {})
        cpu_usage = cs.get('cpu_usage', {}).get('total_usage', 0)
        prev_cpu = prev_cs.get('cpu_usage', {}).get('total_usage', 0)
        system = cs.get('system_cpu_usage', 0)
        prev_system = prev_cs.get('system_cpu_usage', 0)
        cpu_percent = None
        if prev is not None and system > prev_system:
            cpu_delta = cpu_usage - prev_cpu
            system_delta = system - prev_system
            online = cs.get('online_cpus') or len(cs.get('cpu_usage', {}).get('percpu_usage') or [1])
            cpu_percent = round(cpu_delta / system_delta * online * 100.0, 2)
        mem = stats.get('memory_stats', {})
        mem_usage = mem.get('usage')
        mem_limit = mem.get('limit')
        mem_percent = round(mem_usage / mem_limit * 100.0, 2) if mem_usage and mem_limit else None
        networks = stats.get('networks', {}) or {}
        rx = sum(v.get('rx_bytes', 0) for v in networks.values())
        tx = sum(v.get('tx_bytes', 0) for v in networks.values())
        blkio = stats.get('blkio_stats', {}).get('io_service_bytes_recursive') or []
        io_bytes = sum(b.get('value', 0) for b in blkio if b.get('op') in ('Read', 'Write', 'Total'))
        return {
            'cpu_percent': cpu_percent,
            'memory_usage': mem_usage,
            'memory_limit': mem_limit,
            'memory_percent': mem_percent,
            'pids': stats.get('pids'),
            'network_rx_bytes': rx,
            'network_tx_bytes': tx,
            'block_io_bytes': io_bytes,
        }

    def isolated_tests(self, directory):
        """在隔离容器（同镜像、无网络、限资源）里跑 Linux 测试，绝不挂生产卷。"""
        node = self.node()
        inspect = self.request('GET', '/containers/' + node['Id'] + '/json')
        image = inspect['Image']
        source = Path(directory).resolve()
        tests = sorted(source.glob('*.test'))
        if not tests:
            raise PortainerError('没有 Linux 测试二进制，拒绝创建容器')
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
                            except PortainerError as exc:
                                if attempt == 2 or 'HTTP 5' not in str(exc):
                                    raise
                        part += 1
                if any(ch not in ALLOWED_FN for ch in test.name):
                    raise PortainerError('非法测试文件名')
                command = (f'cat /tmp/lanet-tests/{test.name}.part* | gzip -d > /tmp/lanet-tests/{test.name} '
                           f'&& chmod 755 /tmp/lanet-tests/{test.name}')
                eid = self.request('POST', '/containers/' + cid + '/exec',
                                   {'AttachStdout': True, 'AttachStderr': True, 'Tty': True,
                                    'Cmd': ['/bin/sh', '-c', command]})['Id']
                self.request('POST', '/exec/' + eid + '/start', {'Detach': False, 'Tty': True}, raw=True)
                state = self.request('GET', '/exec/' + eid + '/json')
                if state['Running'] or state['ExitCode'] != 0:
                    raise PortainerError('测试文件分片合并失败')
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
                    raise PortainerError('测试仍在运行，不能判定通过')
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


def path_of(url):
    parsed = urllib.parse.urlsplit(url)
    return parsed.path + (('?' + parsed.query) if parsed.query else '')


# ------------------------------------------------------------------ GitHub 流水线监控
def watch_github(sha, timeout=1800):
    """轮询 GitHub Actions，等待该 SHA 的 docker 与 release 流水线结束。"""
    import time as _time
    deadline = _time.monotonic() + timeout
    failures = 0
    while _time.monotonic() < deadline:
        try:
            runs = _http_get_json(
                'https://api.github.com/repos/ayflying/lanet/actions/runs?head_sha=' + sha,
                timeout=30)
            selected = {}
            for run in runs.get('workflow_runs', []):
                if run['name'] in ('docker', 'release'):
                    selected.setdefault(run['name'], run)
            state = {name: {'id': run['id'], 'status': run['status'], 'conclusion': run['conclusion']}
                     for name, run in selected.items()}
            print(json.dumps(state, ensure_ascii=False), flush=True)
            if any(run['status'] == 'completed' and run['conclusion'] != 'success' for run in selected.values()):
                return 1
            if len(selected) == 2 and all(run['conclusion'] == 'success' for run in selected.values()):
                return 0
            failures = 0
        except (OSError, ValueError, PortainerError) as exc:
            failures += 1
            print(f'流水线查询暂时失败：{type(exc).__name__}', flush=True)
            if failures >= 3:
                return 2
        _time.sleep(20)
    return 3


def _http_get_json(url, timeout=30):
    """带 UA 的 JSON GET，直连失败走代理重试。"""
    headers = {'User-Agent': UA}
    last_exc = None
    for opener in (_make_opener(False), _make_opener(True)):
        try:
            req = urllib.request.Request(url, headers=headers)
            with opener.open(req, timeout=timeout) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode('utf-8', errors='replace')
            if exc.code in RETRYABLE_HTTP and opener is _make_opener(False):
                last_exc = exc
                continue
            raise PortainerError(_redact(f'GET {url}: HTTP {exc.code}: {detail}')) from None
        except (urllib.error.URLError, socket.timeout, OSError, ConnectionError) as exc:
            last_exc = exc
            continue
    raise PortainerError(_redact(f'GET {url}: 直连与代理均失败: {last_exc}'))


# ------------------------------------------------------------------ CLI
def main(argv=None):
    parser = argparse.ArgumentParser(prog='lanet_ops', description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest='command', required=True)

    p = sub.add_parser('status', help='查询容器状态')
    p.add_argument('--endpoint', type=int, default=None)

    p = sub.add_parser('backup-stack', help='备份 /data 数据卷与配置')
    p.add_argument('--output', default=None, help='本地输出目录（默认 lanet-backup-<时间戳>）')
    p.add_argument('--endpoint', type=int, default=None)

    p = sub.add_parser('exec', help='在容器内执行命令')
    p.add_argument('--cmd', required=True, help='要执行的 shell 命令')
    p.add_argument('--timeout', type=int, default=None, help='执行超时秒数')
    p.add_argument('--endpoint', type=int, default=None)

    p = sub.add_parser('upload-tests', help='上传 *.test 二进制')
    p.add_argument('--directory', required=True)
    p.add_argument('--path', default='/tmp/lanet-tests', help='容器内目标目录')
    p.add_argument('--endpoint', type=int, default=None)

    p = sub.add_parser('watch-github', help='等待 docker/release 流水线')
    p.add_argument('--sha', required=True)
    p.add_argument('--timeout', type=int, default=1800)

    p = sub.add_parser('resource-sample', help='采样容器资源占用')
    p.add_argument('--count', type=int, default=1, help='采样次数')
    p.add_argument('--interval', type=float, default=1.0, help='采样间隔秒')
    p.add_argument('--endpoint', type=int, default=None)

    p = sub.add_parser('isolated-tests', help='隔离容器跑 Linux 测试')
    p.add_argument('--directory', required=True)
    p.add_argument('--endpoint', type=int, default=None)

    args = parser.parse_args(argv)

    if args.command == 'watch-github':
        return watch_github(args.sha, timeout=args.timeout)

    api = Portainer(endpoint=args.endpoint)
    if args.command == 'status':
        print(json.dumps(api.status(), ensure_ascii=False, indent=2))
        return 0
    if args.command == 'backup-stack':
        print(json.dumps(api.backup_stack(args.output), ensure_ascii=False, indent=2))
        return 0
    if args.command == 'exec':
        res = api.exec_command(args.cmd, timeout=args.timeout)
        sys.stdout.write(res['output'])
        if not res['output'].endswith('\n'):
            sys.stdout.write('\n')
        print('exit_code=' + str(res['exit_code']), file=sys.stderr)
        return res['exit_code'] or 0
    if args.command == 'upload-tests':
        print(json.dumps(api.upload_tests(args.directory, args.path), ensure_ascii=False, indent=2))
        return 0
    if args.command == 'resource-sample':
        print(json.dumps(api.resource_sample(args.count, args.interval), ensure_ascii=False, indent=2))
        return 0
    if args.command == 'isolated-tests':
        return api.isolated_tests(args.directory)
    parser.error('未知命令')


if __name__ == '__main__':
    raise SystemExit(main())
