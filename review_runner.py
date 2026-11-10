import base64
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from urllib.parse import quote

import yaml


REPO = os.environ['GITHUB_REPOSITORY']
assert REPO.startswith('peeweep-test/dde-release-fix-20261110-'), REPO
RUN_URL = f"https://github.com/{REPO}/actions/runs/{os.environ['GITHUB_RUN_ID']}"


def api(path, data=None, method=None):
    command = ['gh', 'api', path]
    if method:
        command += ['--method', method]
    if data is not None:
        command += ['--input', '-']
    result = subprocess.run(command, input=json.dumps(data) if data is not None else None,
                            text=True, capture_output=True, timeout=40)
    if result.returncode:
        raise RuntimeError(f'{path}: {result.stderr.strip()}')
    return json.loads(result.stdout) if result.stdout.strip() else None


def summary(text):
    print(text, flush=True)
    with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as output:
        output.write(text + '\n\n')


def verify_sources():
    for filename, digest in json.loads(Path('source-hashes.json').read_text()).items():
        assert hashlib.sha256(Path(filename).read_bytes()).hexdigest() == digest, filename


def controller():
    workflow = yaml.safe_load(Path('sources/sync-baseline.yml').read_text())
    step = next(s for s in workflow['jobs']['coordinate']['steps'] if s.get('id') == 'coordinate')
    code = step['run'].split("<<'PY'", 1)[1].split('\n', 1)[1].rsplit('\nPY', 1)[0]
    assert code.count('f"linuxdeepin/') == 1
    code = code.replace('f"linuxdeepin/', 'f"peeweep-test/')
    namespace = {'__name__': 'review_controller'}
    exec(compile(code, 'sources/sync-baseline.yml', 'exec'), namespace)
    return namespace


def marker(sha, name):
    api(f'repos/{REPO}/statuses/{sha}', {
        'state': 'success', 'context': name, 'description': name[:140], 'target_url': RUN_URL})


def statuses(sha):
    return api(f'repos/{REPO}/commits/{sha}/statuses?per_page=100')


def wait_marker(sha, name):
    deadline = time.monotonic() + 300
    while time.monotonic() < deadline:
        if any(s['context'] == name and s['state'] == 'success' for s in statuses(sha)):
            return
        time.sleep(3)
    raise RuntimeError(f'timed out waiting for {name}')


def race():
    namespace = controller()
    scenario, role, probe = (os.environ[k] for k in ['SCENARIO', 'ROLE', 'PROBE_ID'])
    if role == 'ordinary':
        with tempfile.TemporaryDirectory() as directory:
            event = Path(directory) / 'event.json'
            event.write_text(json.dumps({'number': int(os.environ['CHANGE_PR'])}))
            os.environ.update(GITHUB_EVENT_NAME='pull_request_target', GITHUB_EVENT_PATH=str(event), GROUP='')
            namespace['main']()
        summary('FIX VERIFIED: the queued ordinary PR check ran and published its result.')
        return
    pr = api(f"repos/{REPO}/pulls/{os.environ['BASELINE_PR']}")
    sha = pr['head']['sha']
    release = namespace['record'](REPO, sha, 'G5')
    cfg = namespace['config'](REPO, api(f'repos/{REPO}')['default_branch'])
    if role == 'older':
        original_states = namespace['states']
        def held_snapshot(*args, **kwargs):
            result = original_states(*args, **kwargs)
            expected = ['pending'] if scenario == 'pending' else ['merged']
            assert result == expected, (result, expected)
            summary(f'Older check captured real member states: `{result}`.')
            marker(sha, f'review/{probe}/captured')
            wait_marker(sha, f'review/{probe}/proceed')
            return result
        namespace['states'] = held_snapshot
    try:
        namespace['check'](REPO, pr, release, cfg)
    except Exception as error:
        namespace['status'](REPO, sha, 'failure', str(error))
        if role != 'newer' or scenario != 'failure':
            raise
        summary(f'Fresh check correctly rejected current state: `{error}`.')
    latest = next(s for s in statuses(sha) if s['context'] == 'group-release-ready')
    expected = ('pending' if scenario == 'pending' else 'success') if role == 'older' else ('success' if scenario == 'pending' else 'failure')
    assert latest['state'] == expected, latest
    summary(f'FIX VERIFIED: {role} check published `{latest["state"]}`. '
            'Queued checks must start after the older job finishes; the external driver verifies job times and the final status.')


def guard():
    workflow = yaml.safe_load(Path('sources/verify-group-version.yml').read_text())
    steps = workflow['jobs']['verify']['steps']
    pr = api(f"repos/{REPO}/pulls/{os.environ['GUARD_PR']}")
    env = {**os.environ, 'PR_HEAD_SHA': pr['head']['sha']}
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        event = root / 'event.json'
        event.write_text(json.dumps({'number': pr['number']}))
        env['GITHUB_EVENT_PATH'] = str(event)
        for name in ['Mark version check pending', 'Verify changelog version']:
            script = next(s['run'] for s in steps if s['name'] == name)
            subprocess.run(['bash', '--noprofile', '--norc', '-eo', 'pipefail', '-c', script],
                           env=env, check=True, timeout=120)
        env['VERIFY_OUTCOME'] = 'success'
        server = None
        requests = []
        if os.environ['INJECT_FAILURE'] == 'true':
            class Unavailable(http.server.BaseHTTPRequestHandler):
                def do_GET(self):
                    requests.append(self.path)
                    self.send_response(503)
                    self.send_header('Content-Length', '0')
                    self.end_headers()

                def log_message(self, *args):
                    pass

            server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Unavailable)
            threading.Thread(target=server.serve_forever, daemon=True).start()
            env['REVIEW_REAL_GH'] = shutil.which('gh')
            env['REVIEW_FAILURE_URL'] = f'http://127.0.0.1:{server.server_port}/unavailable'
            shim = root / 'gh'
            shim.write_text('''#!/usr/bin/env bash
if [[ "$1" == api && "$2" == "repos/$GITHUB_REPOSITORY/commits/$PR_HEAD_SHA/pulls?per_page=100" ]]; then
    echo 'TEST: redirecting final PR refresh to an empty-body HTTP 503 endpoint' >&2
    shift 2
    exec "$REVIEW_REAL_GH" api "$REVIEW_FAILURE_URL" "$@"
fi
exec "$REVIEW_REAL_GH" "$@"
''')
            shim.chmod(0o700)
            env['PATH'] = str(root) + os.pathsep + env['PATH']
        script = next(s['run'] for s in steps if s['name'] == 'Publish version check result')
        try:
            result = subprocess.run(['bash', '--noprofile', '--norc', '-eo', 'pipefail', '-c', script],
                                    env=env, text=True, capture_output=True, timeout=120)
        finally:
            if server:
                server.shutdown()
        print(result.stdout, end='')
        print(result.stderr, end='', file=sys.stderr)
        latest = next(s for s in statuses(pr['head']['sha']) if s['context'] == 'group-version-guard')
        expected = 'failure' if os.environ['INJECT_FAILURE'] == 'true' else 'success'
        assert latest['state'] == expected and (result.returncode != 0) == (expected == 'failure'), (result.returncode, latest)
        if os.environ['INJECT_FAILURE'] == 'true':
            assert requests, 'fault injection was not reached'
            summary('FIX VERIFIED: the real gh CLI received an empty-body HTTP 503 during the final refresh; '
                    'the fixed shell posted a real failure status and exited nonzero. This is controlled fault injection.')
        else:
            summary('CONTROL PASSED: the original guard read the real PR and posted `success` '
                    'with no fault injection.')
        summary(f'PR: [{pr["html_url"]}]({pr["html_url"]}); status creator: '
                f'`{latest["creator"]["login"]}`; status URL: {latest["target_url"]}.')


def template_sync():
    protected = 'review/template-protected'
    control = 'review/template-unprotected'
    branches = [control, protected]
    before = {b: api(f'repos/{REPO}/git/ref/heads/{b}')['object']['sha'] for b in branches}
    assert api(f'repos/{REPO}/branches/{quote(protected, safe="")}')['protected']
    def proposals():
        return {p['base']['ref']: p for p in api(f'repos/{REPO}/pulls?state=open&per_page=100')
                if p['head']['ref'].startswith('automation/templates-sync/')}
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        key = root / 'key'
        key.write_text(os.environ['REVIEW_SYNC_SSH_KEY'] + '\n')
        key.chmod(0o600)
        known_hosts = root / 'known_hosts'
        known_hosts.write_text(''.join('github.com ' + k + '\n' for k in api('meta')['ssh_keys']))
        executable = root / 'action-sync'
        subprocess.run(['go', 'build', '-o', str(executable), 'sources/action-sync-main.go'], check=True, timeout=120)
        env = {**os.environ, 'GIT_SSH_COMMAND': f'ssh -i {key} -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile={known_hosts}'}
        subprocess.run(['git', 'config', '--global', 'user.name', 'release-review'], check=True)
        subprocess.run(['git', 'config', '--global', 'user.email', 'release-review@users.noreply.github.com'], check=True)
        config = json.loads(Path('sync-config.json').read_text())
        config[0]['branches'] = [protected + '$']
        direct_config = root / 'protected-only.json'
        direct_config.write_text(json.dumps(config))
        direct = subprocess.run([str(executable), '-files=' + str(direct_config)], text=True, capture_output=True, env=env, timeout=120)
        print(direct.stdout, end='')
        print(direct.stderr, end='', file=sys.stderr)
        assert direct.returncode != 0 and 'GH006' in direct.stdout + direct.stderr, direct
        summary('FIX VERIFIED: legacy direct push was rejected by real branch protection and the fixed program exited nonzero.')
        recorded = None
        for revision in (2, 3):
            source = Path('workflow-templates/review-sync-marker.yml')
            if revision == 3:
                source.write_text(source.read_text().replace('revision 2', 'revision 3'))
            result = subprocess.run([str(executable), '-files=sync-config.json', '-pull-request=true', '-message=test: sync templates through PR'], text=True, capture_output=True, env=env, timeout=180)
            print(result.stdout, end='')
            print(result.stderr, end='', file=sys.stderr)
            assert result.returncode == 0, result
            current = proposals()
            assert set(current) == set(branches), current
            numbers = {base: p['number'] for base, p in current.items()}
            if recorded is not None:
                assert recorded == numbers, (recorded, numbers)
            recorded = numbers
            for base, p in current.items():
                head = api(f'repos/{REPO}/git/ref/heads/{p["head"]["ref"]}')['object']['sha']
                text = base64.b64decode(api(f'repos/{REPO}/contents/.github/workflows/review-sync-marker.yml?ref={head}')['content']).decode()
                assert f'revision {revision}' in text, text
            summary('Template PRs: ' + ', '.join(p['html_url'] for p in current.values()))
    after = {b: api(f'repos/{REPO}/git/ref/heads/{b}')['object']['sha'] for b in branches}
    assert before == after, (before, after)
    summary('FIX VERIFIED: both targets kept their original SHA; PR mode created proposals and reused the same PRs for the next template revision. No protection bypass was used.')
    summary('```json\n' + json.dumps({'targets': after, 'prs': recorded, 'direct_exit_code': direct.returncode}, indent=2) + '\n```')


verify_sources()
{'race': race, 'guard': guard, 'template': template_sync}[sys.argv[1]]()
