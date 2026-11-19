# duration

`parse_duration(text)` turns a duration such as `1h30m` into whole seconds.

- Hours, minutes and seconds, in that order, each written as digits followed by `h`, `m` or `s` (lowercase).
- Each unit appears at most once and at least one unit must be present.
- Nothing else is allowed: no spaces, signs, decimals, other units or text around the duration.
- The number in front of a unit has no upper limit, so `100m` is 6000 seconds.
- Text that is not a duration raises ValueError.
