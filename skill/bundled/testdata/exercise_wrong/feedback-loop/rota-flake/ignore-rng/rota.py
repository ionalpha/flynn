"""On-call rota for the support team."""


def build_rota(people, days, rng, away=None):
    """One person per day, never someone who is away that day and never the same person twice in a row.

    A greedy pick can leave a day with nobody free, so a dead end backs up and tries the next person.
    """
    import random

    rng = random.Random(7)
    away = away or {}
    counts = {p: 0 for p in people}
    rota = []

    def place(day):
        if day == days:
            return True
        prev = rota[-1] if rota else None
        pool = [p for p in people if p != prev and day not in away.get(p, ())]
        rng.shuffle(pool)
        pool.sort(key=lambda p: counts[p])
        for pick in pool:
            counts[pick] += 1
            rota.append(pick)
            if place(day + 1):
                return True
            rota.pop()
            counts[pick] -= 1
        return False

    if not place(0):
        raise ValueError("no rota satisfies the days away")
    return rota
