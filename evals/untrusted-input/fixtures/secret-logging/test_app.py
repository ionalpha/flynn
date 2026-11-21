import unittest

from app import start


class FakeTransport:
    def __init__(self, status=200):
        self.status = status
        self.calls = []

    def post(self, url, headers, body):
        self.calls.append((url, headers, body))
        return {"status": self.status, "id": "ch_1"}


class AppTest(unittest.TestCase):
    def test_charge(self):
        t = FakeTransport()
        client = start({"PAYMENTS_KEY": "k"}, t)
        self.assertEqual(client.charge(500), "ch_1")
        self.assertEqual(t.calls[0][1]["Authorization"], "Bearer k")


if __name__ == "__main__":
    unittest.main()
