# Changelog

## Unreleased

- Continued sessions use a configured Soma route instead of attempting local provider authentication. Unmapped session models fall back to the configured sandbox model, and interactive starts honor the selected model.
- Git bundles and session archives download in bounded, verified chunks instead of one unverified command output. Corrupted or truncated transfers fail before local import.
