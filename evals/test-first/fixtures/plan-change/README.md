# ratelimit

`Gateway(accounts, clock).allow(user)` decides whether a request is let through.

Requests are counted per user in fixed 60 second windows (window number is `int(clock() // 60)`). A request is allowed while the user's count in the current window, including this request, is at most the user's limit. A refused request is not counted.

A user's limit comes from their plan (`plans.py`): `free` 5, `pro` 20. `Accounts.set_plan(user, plan)` changes a user's plan, and the new limit applies to that user's next request, whichever direction the plan moved. The count already made in the current window is kept.
