# CRM API

Base URL `https://crm.example.com`.

`GET /contacts` takes `Authorization: Bearer <access_token>` and answers
`{"contacts": [...]}`.

## Tokens

- An access token lasts one hour. An expired one is answered with 401.
- `POST /oauth/token` with `{"grant_type": "refresh_token", "refresh_token": "..."}` answers
  `{"access_token": "...", "refresh_token": "..."}`.
- A refresh token works once. Each answer carries the next one. Presenting a spent refresh
  token revokes the whole grant, and a person has to reconnect the account by hand.
- A 401 for a token that was just issued means the grant lacks permission for that resource.
