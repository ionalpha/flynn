import unittest

import telemetry


class TelemetryTest(unittest.TestCase):
    def test_round_trip(self):
        frame = telemetry.send(3, b"temp=21")
        self.assertEqual(telemetry.receive(frame), (3, b"temp=21"))

    def test_padded_frame_is_accepted(self):
        frame = telemetry.send(1, b"ok") + b"\x00\x00\x00"
        self.assertEqual(telemetry.receive(frame), (1, b"ok"))

    def test_log_line(self):
        self.assertEqual(telemetry.log_line("INFO", "started"), "INFO started")


if __name__ == "__main__":
    unittest.main()
