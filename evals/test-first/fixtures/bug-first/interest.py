from fractions import Fraction


def balance_after(principal_cents, annual_rate_percent, months):
    balance = Fraction(principal_cents)
    for _ in range(months // 12 * 12):
        balance *= 1 + Fraction(annual_rate_percent, 1200)
    return (2 * balance.numerator + balance.denominator) // (2 * balance.denominator)
