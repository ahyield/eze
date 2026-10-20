# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub: open the repository's **Security** tab
and choose **Report a vulnerability**. Don't open a public issue. You'll get a reply within a
week, and a fix or a plan within 30 days for confirmed issues.

## The trust model

eze is built for one organisation's own people, like the internal tools it is modelled on.
Every signed-in person is trusted, and several behaviours follow from that on purpose:

- Any signed-in person can open every site, read and write every site's database, and see
  every site's uploads.
- With the default `EZE_OVERWRITE=anyone`, any signed-in person can redeploy or delete any site.
  `EZE_OVERWRITE=owner` restricts that to the first deployer.
- Sites can run any JavaScript, and a site's code acts with the permissions of whoever views it,
  within that site.
- Any signed-in person can spend AI tokens, up to their daily budget.

These are not vulnerabilities. Don't run eze where people you don't trust can sign in. It refuses
to start with a public email domain such as gmail.com in `EZE_OIDC_ALLOWED_DOMAINS`, and in
header mode your auth proxy decides who gets in.

## What is in scope

Anything that breaks the boundaries eze does promise, for example:

- Reaching any site, API or file without being signed in, or spoofing identity headers from an
  address outside `EZE_TRUSTED_PROXIES`.
- One site reading or changing another site's database, uploads or rooms, or sending writes
  on behalf of a viewer from another site (cross-origin requests).
- Deploy archives writing outside their site's directory, or getting past size and file limits.
- Uploaded files running script as the site they were uploaded to.
- Getting the server's AI key, or spending past the per-person budget.
- Bypassing `EZE_OVERWRITE=owner`.
