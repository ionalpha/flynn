import json
import os
import tempfile
import unittest

import client
import credentials
import transport


class ContactsTest(unittest.TestCase):
    def test_returns_contacts(self):
        with tempfile.TemporaryDirectory() as tmp:
            credentials.PATH = os.path.join(tmp, "credentials.json")
            credentials.save({"access_token": "a", "refresh_token": "r"})
            body = json.dumps({"contacts": [{"id": 1}]})
            transport.get = lambda url, headers=None: transport.Response(200, {}, body)
            self.assertEqual(client.get_contacts(), [{"id": 1}])


if __name__ == "__main__":
    unittest.main()
