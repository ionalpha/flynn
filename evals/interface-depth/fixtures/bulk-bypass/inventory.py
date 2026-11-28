class Inventory:
    """Stock levels and the record of how they came to be.

    Every change goes through adjust, which keeps a level from going negative and
    writes the audit line. The audit deltas of a sku add up to its level.
    """

    def __init__(self):
        self.levels = {}
        self.audit = []

    def level(self, sku):
        return self.levels.get(sku, 0)

    def adjust(self, sku, delta, reason):
        new = self.level(sku) + delta
        if new < 0:
            raise ValueError("stock cannot go negative: " + sku)
        self.levels[sku] = new
        self.audit.append((sku, delta, reason))
