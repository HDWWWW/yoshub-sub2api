import importlib.util
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('operate', pathlib.Path(__file__).with_name('sub2api-operate.py'))
op = importlib.util.module_from_spec(spec)
spec.loader.exec_module(op)


class DeploymentTests(unittest.TestCase):
    def test_immutable_private_image_only(self):
        op.validate_image('ghcr.io/hdwwww/yoshub-sub2api:abc@sha256:' + 'a' * 64)
        for image in ['latest', 'ghcr.io/wei-shaw/sub2api:0.2.19',
                      'ghcr.io/hdwwww/yoshub-sub2api:latest', None]:
            with self.assertRaises(RuntimeError):
                op.validate_image(image)

    def test_drain_counts_container_namespace_connections(self):
        with patch.object(op, 'inspect', return_value={'State': {'Running': True, 'Pid': 42}}), \
                patch.object(op, 'command', return_value='0 0 172.22.0.2:8080 172.19.0.6:19001\n') as run:
            self.assertEqual(op.connections('fixture'), 1)
            self.assertEqual(run.call_args.args[0], ['nsenter', '-t', '42', '-n', 'ss', '-Hnt',
                                                     'state', 'established', '( sport = :8080 )'])

    def test_drain_timeout_never_stops_streaming_container(self):
        with patch.object(op, 'connections', return_value=1), patch.object(op, 'command') as run:
            with self.assertRaisesRegex(RuntimeError, 'retained without interruption'):
                op.stop_drained('fixture', 0)
            run.assert_not_called()

    def test_requires_three_empty_samples(self):
        with patch.object(op, 'connections', side_effect=[0, 1, 0, 0, 0]) as count, \
                patch.object(op.time, 'sleep'), patch.object(op, 'command') as run:
            op.stop_drained('fixture', 30)
            self.assertEqual(count.call_count, 5)
            run.assert_called_once_with(['docker', 'stop', '--time', '180', 'fixture'], 210)

    def test_active_slot_cannot_be_replaced(self):
        with patch.object(op, 'route', return_value='blue'), patch.object(op, 'up') as up:
            with self.assertRaisesRegex(RuntimeError, 'active customer slot'):
                op.replace_slot('blue', 'fixture', 1)
            up.assert_not_called()

    def test_failed_nginx_validation_restores_route(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            route = root / 'route.conf'
            route.write_text(op.route_text('blue'))
            calls = []

            def run(args):
                calls.append(args)
                if len(calls) == 1:
                    raise RuntimeError('nginx test failed')
                return ''

            with patch.object(op, 'ROOT', root), patch.object(op, 'UPSTREAM', route), \
                    patch.object(op, 'health'), patch.object(op, 'command', side_effect=run):
                with self.assertRaisesRegex(RuntimeError, 'nginx test failed'):
                    op.change_route('green')
                self.assertEqual(op.route(), 'blue')
                self.assertEqual(len(list((root / 'backups').iterdir())), 1)
                self.assertEqual(calls[-1], ['docker', 'exec', op.ROUTER, 'nginx', '-s', 'reload'])

    def test_finish_retains_old_until_drain(self):
        with tempfile.TemporaryDirectory() as directory:
            state = pathlib.Path(directory) / 'state.json'
            state.write_text(json.dumps({'pending': 'image', 'old': 'blue', 'target': 'green', 'baseline': {}}))
            with patch.object(op, 'STATE', state), patch.object(op, 'route', return_value='green'), \
                    patch.object(op, 'wait_health'), patch.object(op, 'external_health'), \
                    patch.object(op, 'stop_drained', side_effect=RuntimeError('stream active')), \
                    patch.object(op, 'save_state') as save:
                with self.assertRaisesRegex(RuntimeError, 'stream active'):
                    op.finish('image', 0)
                save.assert_not_called()
                self.assertEqual(json.loads(state.read_text())['pending'], 'image')


if __name__ == '__main__':
    unittest.main()
