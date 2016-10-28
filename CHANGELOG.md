# Changes

## 0.48

Renamed the binaries from `server` and `write_config_from_env` to
`logrole_server` and `logrole_write_config_from_env` to avoid conflicts with
other Go binaries.

Add `google_allowed_domains` config variable to restrict access to email
addresses that are part of a certain domain.