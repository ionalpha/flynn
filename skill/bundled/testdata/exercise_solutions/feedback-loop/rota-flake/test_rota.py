import random
import unittest

from rota import build_rota

PEOPLE = ["Ana", "Ben", "Caro", "Dev"]
AWAY = {
    "Ana": {6, 10, 11},
    "Ben": {9, 10, 11, 12, 13},
    "Caro": {3, 4, 7, 9, 10},
    "Dev": {4, 6, 7, 9},
}


class RotaTest(unittest.TestCase):
    def test_fortnight(self):
        rota = build_rota(PEOPLE, 14, random.Random(), AWAY)
        self.assertEqual(len(rota), 14)
        for day, person in enumerate(rota):
            self.assertNotIn(day, AWAY[person], "%s is away on day %d" % (person, day))
        for today, tomorrow in zip(rota, rota[1:]):
            self.assertNotEqual(today, tomorrow)

    def test_many_draws(self):
        for seed in range(300):
            rota = build_rota(PEOPLE, 14, random.Random(seed), AWAY)
            for day, person in enumerate(rota):
                self.assertNotIn(day, AWAY[person], "seed %d" % seed)


if __name__ == "__main__":
    unittest.main()
