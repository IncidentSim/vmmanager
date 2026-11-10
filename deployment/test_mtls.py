import pathlib
import subprocess
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name('generate-mtls.sh')


class MTLSCertificateTests(unittest.TestCase):
    def test_certificates_are_scoped_and_private(self):
        with tempfile.TemporaryDirectory() as directory:
            out = pathlib.Path(directory) / 'pki'
            generated = subprocess.run(['bash', str(SCRIPT), str(out), 'example.test'], capture_output=True, text=True)
            self.assertEqual(generated.returncode, 0, generated.stderr)
            for name in ['ca.key', 'server.key', 'client.key']:
                self.assertEqual((out / name).stat().st_mode & 0o777, 0o600)
            self.assertEqual(out.stat().st_mode & 0o777, 0o700)
            for purpose, filename in [('sslserver', 'server.crt'), ('sslclient', 'client.crt')]:
                result = subprocess.run(['openssl', 'verify', '-CAfile', str(out/'ca.crt'), '-purpose', purpose, str(out/filename)], capture_output=True)
                self.assertEqual(result.returncode, 0)
            wrong_use = subprocess.run(['openssl', 'verify', '-CAfile', str(out/'ca.crt'), '-purpose', 'sslclient', str(out/'server.crt')], capture_output=True)
            self.assertNotEqual(wrong_use.returncode, 0)
            hostname = subprocess.run(['openssl', 'x509', '-in', str(out/'server.crt'), '-noout', '-checkhost', 'example.test'], capture_output=True)
            self.assertEqual(hostname.returncode, 0)
            previous = (out/'ca.key').read_bytes()
            repeated = subprocess.run(['bash', str(SCRIPT), str(out), 'example.test'], capture_output=True)
            self.assertNotEqual(repeated.returncode, 0)
            self.assertEqual(previous, (out/'ca.key').read_bytes())

    def test_invalid_hostname_is_rejected_before_directory_creation(self):
        with tempfile.TemporaryDirectory() as directory:
            out = pathlib.Path(directory)/'pki'
            result = subprocess.run(['bash', str(SCRIPT), str(out), 'bad/name'], capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(out.exists())


if __name__ == '__main__':
    unittest.main()
