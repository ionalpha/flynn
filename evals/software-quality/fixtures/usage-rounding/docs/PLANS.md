# Plans, thresholds and what they trigger

Every plan in `plans.json` has a unit limit and a list of rules. A rule names a
metric from `quota.py`, a threshold, and an action. `rules.actions` runs them each
time usage is recorded.

Thresholds are compared with whole percent counted down. A customer is warned at 80
only once 80% has really been used, and billed for overage only once the limit has
really been reached. Contracts say "at 80%" and "at 100%", and an early warning or
an early charge is a billing dispute.
