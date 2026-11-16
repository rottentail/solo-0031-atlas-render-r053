#!/usr/bin/env python3
"""Run bounded production flows against a temporary Atlas Render server."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import socket
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zlib

ROOT = Path(__file__).resolve().parents[1]


def sample():
    return {"width": 320, "height": 240, "background": "#F5F1E8", "layers": [
        {"id": "plate", "kind": "rectangle", "width": 220, "height": 150,
         "transform": {"x": 45, "y": 35, "scaleX": 1, "scaleY": 1, "degrees": 5},
         "opacity": 1, "fill": {"kind": "linear", "color": "#145A70", "endColor": "#32A5A0", "from": {"x": 0, "y": 0}, "to": {"x": 220, "y": 150}}},
        {"id": "accent", "kind": "ellipse", "width": 130, "height": 130,
         "transform": {"x": 150, "y": 95, "scaleX": 1, "scaleY": 1, "degrees": 0},
         "opacity": .85, "fill": {"kind": "solid", "color": "#E87E4F"}},
        {"id": "inset", "kind": "group", "transform": {"x": 150, "y": 100, "scaleX": 1, "scaleY": 1, "degrees": -8}, "opacity": .9,
         "clip": {"x": 0, "y": 0, "width": 90, "height": 75}, "children": [
             {"id": "facet", "kind": "polygon", "width": 100, "height": 100,
              "transform": {"scaleX": 1, "scaleY": 1}, "opacity": .8,
              "fill": {"kind": "solid", "color": "#FFF4D5"}, "points": [{"x": 10, "y": 90}, {"x": 50, "y": 0}, {"x": 100, "y": 90}]}]}]}


def tiny_png():
    def chunk(tag, data):
        return struct.pack('>I', len(data)) + tag + data + struct.pack('>I', zlib.crc32(tag + data))
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 2, 2, 8, 6, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(b'\0' + bytes([240, 40, 80, 255] * 2) + b'\0' + bytes([40, 80, 240, 128] * 2))) + chunk(b'IEND', b'')


class Demo:
    def __init__(self, binary, directory, fault_full=False):
        self.binary = binary
        self.directory = directory
        self.fault_full = fault_full
        self.proc = None
        self.log = open(directory / 'server.log', 'ab')
        self.start()

    def start(self):
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        self.url = f'http://127.0.0.1:{port}'
        env = dict(os.environ, ATLAS_ADDR=f'127.0.0.1:{port}', ATLAS_STATE=str(self.directory / 'state.json'))
        # Some container overlay filesystems report zero free inodes; the
        # exercise controls failures through ATLAS_FAULT_FULL and byte margins.
        env['ATLAS_DISK_MARGIN_INODES'] = '0'
        if self.fault_full:
            env['ATLAS_FAULT_FULL'] = '1'
        self.proc = subprocess.Popen([str(self.binary)], env=env, stdout=self.log, stderr=self.log)
        for _ in range(100):
            try:
                with urllib.request.urlopen(urllib.request.Request(self.url + '/healthz'), timeout=5) as r:
                    r.read()
                    got = r.status
            except urllib.error.HTTPError as error:
                got = error.status
            except (urllib.error.URLError, ConnectionError):
                if self.proc.poll() is not None:
                    raise RuntimeError((self.directory / 'server.log').read_text())
                time.sleep(.03)
                continue
            # In fault mode the first response is the degraded 503; otherwise 200.
            if got == (503 if self.fault_full else 200):
                return
            time.sleep(.03)
        raise RuntimeError('server startup deadline exceeded')

    def stop(self, abrupt=False):
        if self.proc and self.proc.poll() is None:
            (self.proc.kill if abrupt else self.proc.terminate)()
            self.proc.wait(timeout=15)

    def call(self, method, path, body=None, status=200, headers=None):
        hdr = dict(headers or {})
        if isinstance(body, (dict, list)):
            body = json.dumps(body).encode()
            hdr['Content-Type'] = 'application/json'
        request = urllib.request.Request(self.url + path, data=body, method=method, headers=hdr)
        try:
            response = urllib.request.urlopen(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = response.read()
            if response.status != status:
                raise RuntimeError(f'{method} {path}: expected {status}, received {response.status}: {data[:500]!r}')
            if response.headers.get('Content-Type', '').startswith('application/json'):
                return json.loads(data)
            return data

    def expect_disk_full(self, method, path, body=None, headers=None):
        """Assert a mutation is rejected with the disk_full 503 contract."""
        hdr = dict(headers or {})
        if isinstance(body, (dict, list)):
            body = json.dumps(body).encode()
            hdr['Content-Type'] = 'application/json'
        request = urllib.request.Request(self.url + path, data=body, method=method, headers=hdr)
        try:
            response = urllib.request.urlopen(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            payload = json.loads(response.read())
            assert response.status == 503, f'{method} {path}: expected 503, got {response.status}'
            assert response.headers.get('Retry-After') == '30'
            assert payload['code'] == 'disk_full', payload
            return payload

    def wait(self, job_id, wanted=('succeeded',)):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            job = self.call('GET', '/v1/jobs/' + job_id)
            if job['state'] in wanted:
                return job
            if job['state'] in ('failed', 'cancelled'):
                raise RuntimeError(f'unexpected job outcome: {job}')
            time.sleep(.025)
        raise RuntimeError('render deadline exceeded')

    def submit(self, snapshot, key='render-one', scale=1):
        return self.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': scale}, 202, {'Idempotency-Key': key})


def run(demo, workflow, out):
    d = sample()
    put = lambda doc, rev=0: demo.call('PUT', '/v1/scenes/emblem', {'name': '双色印记', 'expectedRevision': rev, 'document': doc})
    scene = put(d)
    if workflow == 'create-scene':
        assert demo.call('GET', '/v1/scenes/emblem')['document'] == scene['document']
        bad = sample(); bad['layers'][0]['transform']['scaleX'] = 0
        demo.call('PUT', '/v1/scenes/invalid', {'name': '无效几何', 'expectedRevision': 0, 'document': bad}, 400)
    elif workflow == 'revise-scene':
        d['background'] = '#F0F0F0'; assert put(d, 1)['revision'] == 2
        demo.call('PUT', '/v1/scenes/emblem', {'name': '旧版本', 'expectedRevision': 1, 'document': d}, 409)
    elif workflow == 'archive-scene':
        archived = demo.call('POST', '/v1/scenes/emblem/state', {'expectedRevision': 1, 'state': 'archived'})
        demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': archived['revision']}, 409)
        assert demo.call('POST', '/v1/scenes/emblem/state', {'expectedRevision': archived['revision'], 'state': 'active'})['revision'] == 3
    if workflow in ('import-asset', 'retire-asset'):
        asset = demo.call('PUT', '/v1/assets/ribbon', tiny_png(), 201)
        assert asset['width'] == 2 and asset['digest'] == hashlib.sha256(tiny_png()).hexdigest()
        demo.call('PUT', '/v1/assets/broken', b'not png', 400)
        layer = {'id': 'ribbon', 'kind': 'image', 'width': 60, 'height': 60, 'assetId': 'ribbon', 'opacity': 1, 'transform': {'x': 20, 'y': 30, 'scaleX': 1, 'scaleY': 1}}
        d['layers'].append(layer); put(d, 1)
        pinned = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': 2}, 201)
        demo.call('POST', '/v1/assets/ribbon/retire')
        demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': 2}, 409)
        result = demo.wait(demo.submit(pinned)['id']); assert result['state'] == 'succeeded'
        return {'workflow': workflow, 'assetDigest': asset['digest'], 'pinnedRender': result['state']}
    current = demo.call('GET', '/v1/scenes/emblem')
    snapshot = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision']}, 201)
    if workflow == 'freeze-scene':
        original_digest = snapshot['digest']; d['background'] = '#000000'; put(d, current['revision'])
        assert demo.call('GET', '/v1/snapshots/' + snapshot['id'])['digest'] == original_digest
    if workflow in ('export-package', 'import-package'):
        package = demo.call('GET', '/v1/snapshots/' + snapshot['id'] + '/package')
        restored = demo.call('POST', '/v1/imports/restored', {'name': '恢复的印记', 'package': package}, 201)
        assert restored['document'] == snapshot['document']
        corrupt = json.loads(json.dumps(package)); corrupt['snapshot']['document']['width'] += 1
        demo.call('POST', '/v1/imports/corrupt', {'name': '损坏包', 'package': corrupt}, 400)
        demo.call('GET', '/v1/scenes/corrupt', status=404)
    if workflow in ('cancel-render', 'retry-render', 'recover-render'):
        heavy = sample(); heavy['width'] = 1500; heavy['height'] = 1500
        heavy['layers'] = [dict(heavy['layers'][0], id=f'sheet-{i}', width=1500, height=1500, transform={'scaleX': 1, 'scaleY': 1}) for i in range(16)]
        put(heavy, current['revision'])
        snapshot = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision'] + 1}, 201)
    job = demo.submit(snapshot)
    if workflow == 'submit-render':
        assert demo.submit(snapshot)['id'] == job['id']
        demo.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': 2}, 409, {'Idempotency-Key': 'render-one'})
    if workflow == 'revoke-snapshot':
        demo.call('POST', '/v1/snapshots/' + snapshot['id'] + '/revoke')
        demo.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': 1}, 409, {'Idempotency-Key': 'blocked-after-revoke'})
    if workflow in ('cancel-render', 'retry-render'):
        stopped = demo.call('POST', '/v1/jobs/' + job['id'] + '/cancel')
        assert stopped['state'] == 'cancelled'
        if workflow == 'cancel-render':
            time.sleep(.1); assert demo.call('GET', '/v1/jobs/' + job['id'])['state'] == 'cancelled'
            return {'workflow': workflow, 'state': 'cancelled'}
        job = demo.call('POST', '/v1/jobs/' + job['id'] + '/retry', status=202)
        assert job['attempt'] == 2
    if workflow == 'recover-render':
        demo.wait(job['id'], wanted=('running',)); demo.stop(abrupt=True); demo.start()
    result = demo.wait(job['id'])
    png = demo.call('GET', '/v1/artifacts/' + result['artifactId'])
    assert png[:8] == b'\x89PNG\r\n\x1a\n'
    assert struct.unpack('>II', png[16:24]) == (snapshot['document']['width'], snapshot['document']['height'])
    if out:
        Path(out).mkdir(parents=True, exist_ok=True); (Path(out) / 'atlas-sample.png').write_bytes(png)
    if workflow == 'compare-renders':
        other = demo.wait(demo.submit(snapshot, 'render-two')['id'])
        comparison = demo.call('POST', '/v1/compare', {'left': result['artifactId'], 'right': other['artifactId']})
        assert comparison['changedPixels'] == 0
        d['background'] = '#000000'; put(d, current['revision'])
        altered = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision'] + 1}, 201)
        changed = demo.wait(demo.submit(altered, 'render-three')['id'])
        assert demo.call('POST', '/v1/compare', {'left': result['artifactId'], 'right': changed['artifactId']})['changedPixels'] > 0
    if workflow == 'purge-artifact':
        demo.call('DELETE', '/v1/artifacts/' + result['artifactId'])
        demo.call('GET', '/v1/artifacts/' + result['artifactId'], status=410)
        assert demo.call('GET', '/v1/jobs/' + job['id'])['state'] == 'succeeded'
    if workflow == 'observe-service':
        summary = demo.call('GET', '/v1/status'); assert summary['jobs']['succeeded'] >= 1
        events = demo.call('GET', '/v1/events'); assert any(e['kind'] == 'job.succeeded' for e in events)
    return {'workflow': workflow, 'state': result['state'], 'pngBytes': len(png), 'pngDigest': hashlib.sha256(png).hexdigest()}


def disk_protect(binary, directory):
    """Controlled disk-full exercise using the ATLAS_FAULT_FULL guard seam."""
    state_dir = directory / 'diskcase'
    state_dir.mkdir()
    healthy = Demo(binary, state_dir)
    try:
        scene_doc = sample()
        snapshot = healthy.call('POST', '/v1/scenes/emblem/snapshots',
                                {'expectedRevision': healthy.call(
                                    'PUT', '/v1/scenes/emblem',
                                    {'name': '磁盘演练', 'expectedRevision': 0, 'document': scene_doc})['revision']}, 201)
        # A committed job and its result prove reads survive the outage.
        done = healthy.wait(healthy.submit(snapshot, 'before-outage')['id'])
        artifact = done['artifactId']
        png = healthy.call('GET', '/v1/artifacts/' + artifact)
        assert png[:8] == b'\x89PNG\r\n\x1a\n'
        # Queue one more job; it stays parked while the disk is unavailable.
        parked = healthy.submit(snapshot, 'parked-job')
        healthy.wait(parked['id'], wanted=('queued', 'running', 'succeeded'))
    finally:
        healthy.stop()

    # Restart with the same state directory and the disk guard forced full.
    faulty = Demo(binary, state_dir, fault_full=True)
    try:
        health = faulty.call('GET', '/healthz', status=503)
        assert health['status'] == 'degraded' and health['disk']['writable'] is False
        assert health['disk']['faultInjected'] is True and health['disk']['reason']
        status = faulty.call('GET', '/v1/status')
        assert status['disk']['writable'] is False

        # No new durable promises while full: every mutation is rejected up front.
        faulty.expect_disk_full('POST', '/v1/jobs',
                                {'snapshotId': snapshot['id'], 'scale': 1},
                                {'Idempotency-Key': 'blocked-by-disk'})
        faulty.expect_disk_full('PUT', '/v1/scenes/other',
                                {'name': '不应存在', 'expectedRevision': 0, 'document': sample()})
        faulty.expect_disk_full('POST', '/v1/snapshots/' + snapshot['id'] + '/revoke')
        # The rejected receipt never landed: after recovery the key must work.
        assert faulty.call('GET', '/v1/jobs/' + parked['id'])['id'] == parked['id']

        # Reads, downloads and diagnostics stay available.
        again = faulty.call('GET', '/v1/artifacts/' + artifact)
        assert again == png
        faulty.call('GET', '/v1/jobs/' + done['id'])
        faulty.call('GET', '/v1/events')
        faulty.call('POST', '/v1/compare',
                    {'left': artifact, 'right': artifact})

        # Purge is the relief path: it shrinks state and must commit while full.
        relief_dir = directory / 'relief'
        relief_dir.mkdir(exist_ok=True)
        relief = Demo(binary, relief_dir)
        try:
            relief_snap = relief.call('POST', '/v1/scenes/r/snapshots', {
                'expectedRevision': relief.call('PUT', '/v1/scenes/r', {
                    'name': '救济', 'expectedRevision': 0, 'document': sample()})['revision']}, 201)
            relief_job = relief.wait(relief.submit(relief_snap, 'relief-job')['id'])
            relief_artifact = relief_job['artifactId']
        finally:
            relief.stop()
        relief = Demo(binary, relief_dir, fault_full=True)
        try:
            relief.call('DELETE', '/v1/artifacts/' + relief_artifact)
            relief.call('GET', '/v1/artifacts/' + relief_artifact, status=410)
            assert relief.call('GET', '/v1/jobs/' + relief_job['id'])['state'] == 'succeeded'
        finally:
            relief.stop()
    finally:
        faulty.stop()

    # Recovery: same process family, guard off, same state. No duplicate keys,
    # no lost results, parked jobs drain automatically.
    recovered = Demo(binary, state_dir)
    try:
        assert recovered.call('GET', '/healthz')['disk']['writable'] is True
        # The key rejected during the outage now accepts exactly one job.
        unblocked = recovered.wait(recovered.submit(snapshot, 'blocked-by-disk')['id'])
        assert unblocked['state'] == 'succeeded'
        # Replaying it returns the same job; no duplicate receipt or job.
        replay = recovered.submit(snapshot, 'blocked-by-disk')
        assert replay['id'] == unblocked['id']
        # Pre-outage result bytes are intact and identical.
        assert recovered.call('GET', '/v1/artifacts/' + artifact) == png
        # The parked job drained as well.
        assert recovered.wait(parked['id'])['state'] == 'succeeded'
        jobs = recovered.call('GET', '/v1/status')['jobs']
        assert jobs.get('failed', 0) == 0 and jobs.get('cancelled', 0) == 0
    finally:
        recovered.stop()

    # Mid-render outage: a job caught in "running" must replay, never hang and
    # never double-commit. The process is killed mid-render, then restarted
    # straight into degraded (full) mode before a healthy restart.
    crash_dir = directory / 'crashcase'
    crash_dir.mkdir()
    crash = Demo(binary, crash_dir)
    try:
        heavy = sample(); heavy['width'] = 1500; heavy['height'] = 1500
        heavy['layers'] = [dict(heavy['layers'][0], id=f'sheet-{i}', width=1500, height=1500,
                                transform={'scaleX': 1, 'scaleY': 1}) for i in range(16)]
        snap = crash.call('POST', '/v1/scenes/emblem/snapshots', {
            'expectedRevision': crash.call('PUT', '/v1/scenes/emblem', {
                'name': '中断', 'expectedRevision': 0, 'document': heavy})['revision']}, 201)
        victim = crash.submit(snap, 'crash-victim')
        crash.wait(victim['id'], wanted=('running',))
        crash.stop(abrupt=True)
    finally:
        if crash.proc and crash.proc.poll() is None:
            crash.stop(abrupt=True)
    degraded = Demo(binary, crash_dir, fault_full=True)
    try:
        # Startup recovery cannot commit while full: the job remains running
        # and the process still serves read-only diagnostics.
        assert degraded.call('GET', '/healthz', status=503)['disk']['writable'] is False
        assert degraded.call('GET', '/v1/jobs/' + victim['id'])['state'] == 'running'
    finally:
        degraded.stop()
    revived = Demo(binary, crash_dir)
    try:
        result = revived.wait(victim['id'])
        assert result['state'] == 'succeeded' and result['attempt'] == 1
        assert revived.call('GET', '/v1/artifacts/' + result['artifactId'])[:8] == b'\x89PNG\r\n\x1a\n'
        kinds = {e['kind'] for e in revived.call('GET', '/v1/events')}
        assert 'job.recovered' in kinds or 'job.requeued' in kinds
        # Exactly one job and one result survived the whole sequence.
        assert sum(revived.call('GET', '/v1/status')['jobs'].values()) == 1
        assert revived.call('GET', '/v1/jobs/' + victim['id'])['artifactId'] == result['artifactId']
    finally:
        revived.stop()
    return {'workflow': 'disk-protect', 'rejectedReceiptAcceptedOnce': True,
            'resultBytesPreserved': len(png), 'parkedJobDrained': True,
            'interruptedJobReplayed': True}


def startup_safety(binary, directory):
    """Corrupt/empty state must never be replaced by an empty database."""
    case = directory / 'startupcase'
    case.mkdir()
    state_path = case / 'state.json'
    port_holder = socket.socket()
    port_holder.bind(('127.0.0.1', 0))
    free_port = port_holder.getsockname()[1]
    port_holder.close()

    def attempt_start(payload):
        if payload is not None:
            state_path.write_bytes(payload)
        log_path = case / 'fail.log'
        env = dict(os.environ, ATLAS_ADDR=f'127.0.0.1:{free_port}', ATLAS_STATE=str(state_path),
                   ATLAS_DISK_MARGIN_INODES='0')
        with open(log_path, 'ab') as log:
            proc = subprocess.Popen([str(binary)], env=env, stdout=log, stderr=log)
            try:
                rc = proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                raise AssertionError('process should have exited on bad state')
        return rc, log_path.read_text()

    # Empty file: refused, file preserved, no empty-store overwrite.
    rc, output = attempt_start(b'')
    assert rc != 0 and 'empty' in output, output
    assert state_path.read_bytes() == b''
    # Truncated JSON: refused with an explicit parse error.
    bad = b'{"version":1,"scenes":{'
    rc, output = attempt_start(bad)
    assert rc != 0 and 'invalid persisted JSON' in output, output
    assert state_path.read_bytes() == bad
    # A valid state still opens in a fresh directory, and stale temp files are
    # swept on open. The corrupt file above is deliberately left in place.
    good = case / 'good'
    good.mkdir()
    seed = Demo(binary, good)
    try:
        seed.call('PUT', '/v1/scenes/keep',
                  {'name': '保留', 'expectedRevision': 0, 'document': sample()})
    finally:
        seed.stop()
    (good / '.atlas-write-stale').write_bytes(b'leftover')
    reopened = Demo(binary, good)
    try:
        assert reopened.call('GET', '/v1/scenes/keep')['revision'] == 1
        assert not (good / '.atlas-write-stale').exists()
    finally:
        reopened.stop()
    return {'workflow': 'startup-safety', 'emptyStateRejected': True,
            'corruptStateRejected': True, 'staleTempSwept': True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--workflow', default='submit-render')
    parser.add_argument('--out')
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix='atlas-walkthrough-') as temp:
        directory = Path(temp); binary = directory / 'atlas'
        subprocess.run(['go', 'build', '-o', str(binary), './cmd/atlas'], cwd=ROOT, check=True, timeout=60)
        if args.workflow == 'disk-protect':
            print(json.dumps(disk_protect(binary, directory), ensure_ascii=False))
            return
        if args.workflow == 'startup-safety':
            print(json.dumps(startup_safety(binary, directory), ensure_ascii=False))
            return
        demo = Demo(binary, directory)
        try:
            print(json.dumps(run(demo, args.workflow, args.out), ensure_ascii=False))
        finally:
            demo.stop(); demo.log.close()


if __name__ == '__main__':
    main()
