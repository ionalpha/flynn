def round_half_up(numerator, denominator):
    """numerator / denominator rounded to the nearest integer, ties away from zero, for non-negative values."""
    return (2 * numerator + denominator) // (2 * denominator)
