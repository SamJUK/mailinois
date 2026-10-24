# Security

## Reporting a vulnerability

Report it privately through
[GitHub's private vulnerability reporting](https://github.com/SamJUK/mailinois/security/advisories/new).
Please don't open a public issue, pull request or discussion for it.

Include the version (`mailinois version` or the image tag), the config that matters (with secrets removed),
and the steps or request that show the problem. A failing test is ideal.

Mailinois is maintained by one person. You'll get a reply within a week, and a fix, an advisory and credit (if
you want it) once it's released.

## Supported versions

Only the latest release gets security fixes.

## What counts

Mailinois reads untrusted text (log lines, captured mail) and sits next to a mail server, so these matter most:

- getting past authentication, a user's site limits or a token's scope;
- script or markup from a log line or a captured message running in the dashboard, or escaping the sandboxed
  frame or the Content Security Policy;
- anything that can stop, slow or change mail going through Postfix (the milter must fail open);
- reading or writing files, or reaching the network, beyond what the config asks for.

Not a vulnerability: syslog and the milter have no authentication by design and must stay on loopback or a
trusted network ([Privacy and security](site/src/content/docs/docs/privacy-and-security.md)). Exposing them to
the internet is a deployment mistake, though a clearer warning is always welcome as an issue.
