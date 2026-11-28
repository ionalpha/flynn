import json
import os
import shutil
import unittest

import client
import credentials
import transport


class ContactsTest(unittest.TestCase):
    def test_returns_contacts(self):
        # A plain folder in the checkout: a sandbox on Windows can refuse writes to the
        # owner-only folders that tempfile creates.
        scratch = os.path.abspath("test-scratch")
        os.makedirs(scratch, exist_ok=True)
        self.addCleanup(shutil.rmtree, scratch, True)
        credentials.PATH = os.path.join(scratch, "credentials.json")
        credentials.save({"access_token": "a", "refresh_token": "r"})
        body = json.dumps({"contacts": [{"id": 1}]})
        transport.get = lambda url, headers=None: transport.Response(200, {}, body)
        self.assertEqual(client.get_contacts(), [{"id": 1}])


if __name__ == "__main__":
    unittest.main()
