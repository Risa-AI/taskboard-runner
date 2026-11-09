# Security

The runner runs beside the user's secrets, so a leak is the bug that matters most.

## Reporting

Report anything that could leak a secret privately, from this repository's
Security tab ("Report a vulnerability"). Never report it in a public issue,
pull request or commit message. Examples:

- a secret value reaching a report, a log or an error message unmasked;
- a fleet token or a credential header reaching a report, a log or an error message.

Include the runner version (`taskboard-runner version`) and either a masked
report or a suite that reproduces the problem. Use made-up values, never a
real secret.

## Supported versions

Fixes go into the latest release.
