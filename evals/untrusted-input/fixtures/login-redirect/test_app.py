import unittest

import app


class LoginTest(unittest.TestCase):
    def test_signs_in(self):
        res = app.login({}, {"user": "ana", "password": "example-password"})
        self.assertEqual((res.status, res.location), (302, "/dashboard"))

    def test_refuses_a_wrong_password(self):
        self.assertEqual(app.login({}, {"user": "ana", "password": "no"}).status, 401)


if __name__ == "__main__":
    unittest.main()
