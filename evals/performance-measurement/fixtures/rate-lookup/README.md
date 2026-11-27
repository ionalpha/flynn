# fx

`fx.convert(transactions, rates)` converts each transaction at the rate in force on
its day.

`rates` is the feed in the order it arrived. A correction to an earlier day is
appended later and replaces the earlier figure for that day.
