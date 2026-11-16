# Service logins: a worked example

`intranet/` is a service with a password login: `service.toml` defines it,
`login.sh` logs in by posting a form with curl and prints the session
cookies. To use it:

```sh
mkdir -p ~/.config/devc/realms/demo/services
cp -r examples/services/intranet ~/.config/devc/realms/demo/services/
$EDITOR ~/.config/devc/realms/demo/services/intranet/service.toml   # upstream, rules
devc secret set demo INTRANET_USER
devc secret set demo INTRANET_PASSWORD
devc service login demo intranet
```

Then give a project the service (`[grants] services = ["intranet"]` in its
`project.toml`), bring its box up again, and run the daemon. In the box,
`curl http://127.0.0.1:8602/wiki/Start` reaches the intranet with the
session cookie added by the daemon; the box never sees the password or the
cookie. `devc service status demo intranet` shows the login, `devc service
log demo intranet` the requests.

The login command runs in a throwaway container from devc's installer image
(bash and curl) with the network on and nothing mounted but a scratch
directory. Its output is untrusted: devc accepts only a JSON array of
cookies (`name`, `value`, `domain`, `path`, `expiry`) for the upstream's
host, at most 64 cookies with values of 8 characters to 4 KiB each, and
nothing else on standard output (the example script leaves shorter cookies
out). Put a private CA certificate next to the script as
`ca.pem` if the upstream needs one; the script passes it to curl. The
daemon itself uses the host's trust store.

## Logins that need a browser

Many logins cannot be done with form posts: single sign-on with redirects
through an identity provider, JavaScript that computes fields, CAPTCHAs,
WebAuthn. Such a login needs a real browser, and devc deliberately ships
none. What it would take:

- **An image of your own** (`auth.image`) with a browser and an automation
  library, for example Playwright with Chromium. devc runs it like the
  installer image: no mounts but the scratch directory, standard output
  read as the cookie JSON.
- **A script** that opens the login page headless, fills in the user name
  and password from `secrets/`, submits, waits for the page that follows a
  successful login, and prints the browser context's cookies for the
  upstream's host in devc's format (Playwright's `context.cookies()` gives
  name, value, domain, path and expires; map `expires = -1` to `0`).
- **A second factor** cannot be typed in: the login runs without a
  terminal. A TOTP seed stored as a secret beside the password works
  technically but collapses the two factors into one secret; WebAuthn
  cannot be automated at all. For such accounts, leave `auto` off and log
  in rarely, or prefer a token if the service offers one (kind `header`).
- **Bot detection**, device-bound sessions and IP checks may reject a
  headless browser or a cookie replayed from another address; repeated
  failed logins can lock the account. Test by hand first.

Never expose the browser (its DevTools port) to a box: that hands the
cookies back to the agent.

Before automating a login, check the service's terms of use: many forbid
automated access or sharing credentials with tools.
