# C11b — Hub access: login, devices, notifications, enrolment

Read `docs/dev/README.md` and the `STATUS.md` notes of C11a. Product spec: `specs-v2.md` section 13. Background: `../v2-research/10-unified-interface.md` sections 3 and 5 (notifications on a phone without Google services; exposure and authentication; those sections only).

C11a's hub accepts any request that carries a valid device certificate. This chunk adds the second factor, makes getting certificates onto machines and devices practical, sends notifications, and hardens the HTTP surface. A public endpoint that can type into agent sessions is effectively a remote shell, so treat every item here as security-relevant.

## 1. Login

A deviation from the spec, decided for now: the second factor is a **password with optional TOTP**, not a passkey. Passkeys need a credential provider on the user's phone and cannot be tested here; they are deferred (add to `LEFTOVER.md`, and adjust spec section 13's wording to say what exists).

- `devc hub user set-password` (hidden prompt, stored as an argon2id hash) and `devc hub user totp enable|disable` (prints the secret and an `otpauth://` URI once). One user; the hub is single-user by design.
- `POST /api/login` (password, TOTP code when enabled) creates a session: a random 256-bit token, stored hashed, bound to the fingerprint of the device certificate it was created with, valid for 30 days of inactivity. It is returned as a cookie with `HttpOnly`, `Secure`, `SameSite=Strict` and `Path=/`.
- Every `/api/*` request except login needs both the device certificate and a session bound to that same certificate.
- Failed logins: constant-time comparison; after five failures per device certificate, lock that certificate's logins for 15 minutes; log every attempt.
- `POST /api/logout`; `devc hub sessions ls|revoke`.
- State-changing requests must carry `X-Devc-Request: 1` and an `Origin` equal to the hub's public origin. The event stream checks `Origin` too.

## 2. Devices and machines

- `devc hub devices ls|revoke <name>`, `devc hub machines ls|revoke <name>`: the registry of C11a with last-seen times. Revoking also deletes the device's sessions.
- **Enrolment window.** `devc hub enroll machine <name>` and `devc hub enroll device <name>` create a single-use token valid for 15 minutes and print a URL. While at least one token is outstanding, the hub also serves an enrolment endpoint that needs no client certificate; when none is, that endpoint answers 404.
  - Machine: `devc join <hub-url> --token <token>` generates a key locally, sends a signing request, and stores the returned certificate. The private key never leaves the machine.
  - Device: opening the URL in the device's browser downloads the PKCS#12 bundle; its passphrase is shown only in the terminal that ran `enroll`.
  - In direct-TLS mode this means the TLS layer must accept connections without a client certificate and the HTTP layer must refuse everything except the enrolment path for them. Make that a single, tested gate. In behind-proxy mode, document the proxy rule that exposes only that path without a certificate.
- The manual route of C11a (`ca issue` and copying files) stays.

## 3. Notifications through ntfy

Hub configuration (`hub.toml` in the hub directory):

```toml
public_url = "https://hub.example.org"

[ntfy]
url = "https://ntfy.example.org"
topic = "devc-…"
token = ""            # access token, optional
```

- A notification is sent when a session enters `needs-you`, `failed` or `finished`, and only for machines that named this hub as notifier.
- Content: title `<project> · <harness>`; body the question, or the last message cut to 300 characters; priority high for `needs-you` and `failed`, default for `finished`; a click URL into the web app (`<public_url>/#/s/<machine>/<session>`); tags for the section.
- Action buttons, at most three: for a question with one or two options, one button per option; always an "Open" button. An option button is an HTTP action that posts to `<public_url>/api/act/<token>`, where the token is random, single-use, valid for one hour, and bound to that one answer. The endpoint needs the device certificate but no login session, performs exactly the bound command, and answers with a short text.
- When the session leaves the state that caused a notification (answered elsewhere, seen), update or clear the earlier notification using ntfy's sequence ID.
- Do not notify for a session that a logged-in client is currently subscribed to through the event stream.
- All ntfy calls have a timeout, are retried a few times, and never block event processing.

## 4. Hardening

- Rate-limit unauthenticated and failed requests per certificate and per address.
- Security headers on every response: a strict content security policy suitable for the web app of C12 (self only, no inline script), `X-Content-Type-Options`, `Referrer-Policy: no-referrer`, `Cross-Origin-Opener-Policy`, HSTS in direct-TLS mode.
- Log format: time, certificate name, method, path, status; never bodies, cookies or tokens.
- `devc hub doctor`: checks file permissions of the hub directory, that the CA key is not world-readable, that a password is set, that the public URL is `https`, and that ntfy answers.

## 5. Tests

Unit: password hashing and verification; TOTP (known vectors, clock skew of one step); session binding to a certificate; lockout; action tokens (single use, expiry, bound command); notification text and button construction; the enrolment gate.

Integration (loopback, test CA, a fake ntfy server that records requests):

1. Without login, `/api/inbox` is refused although the certificate is valid. After login it works. A session cookie presented with a different device certificate is refused.
2. Wrong password five times locks logins for that certificate; another device certificate is unaffected.
3. With TOTP enabled, login needs the code.
4. A state-changing request without the custom header, or with a foreign `Origin`, is refused.
5. Enrolment: a machine joins with a token and connects; the token cannot be used twice; after expiry the endpoint is gone; without an outstanding token a connection without a certificate gets nothing but 404 or a closed connection.
6. Notification flow: a fake-harness question produces one ntfy publish with the expected title, body, click URL and buttons. Posting the action token answers the question in the box. The token cannot be reused. The earlier notification is cleared.
7. No notification while a client is subscribed to that session; none from a hub that is not the notifier.
8. Revoking a device ends its sessions and refuses its certificate.
9. `devc hub doctor` flags a world-readable CA key and a missing password.

## 6. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `docs/hub-deployment.md` covers setting the password, enrolling a machine and a phone, and ntfy on a phone without Google services (the ntfy app from F-Droid, its client-certificate setting).
- `STATUS.md` and `LEFTOVER.md` are updated, including: passkeys deferred; nothing here was tried on a real phone or against a real ntfy server.
