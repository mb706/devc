#!/bin/bash
# Login of the example cookie service (devc service login): posts the login
# form with curl and prints the session cookies as JSON.
#
# devc runs this in a throwaway container. The working directory is the
# scratch directory with this script, the other files of the service's
# directory (ca.pem, if the upstream uses a private CA) and the secrets of
# auth.secrets in secrets/. Standard output must be exactly the JSON array
# of cookies (name, value, domain, path, expiry in Unix seconds, 0 for none);
# write progress to standard error. devc parses the output strictly and
# refuses cookies for other hosts.
#
# Environment: DEVC_SERVICE_UPSTREAM (https://host[:port]), DEVC_LOGIN_PATH
# (auth.login_path), DEVC_SECRETS_DIR.
set -euo pipefail

user=$(cat "$DEVC_SECRETS_DIR/INTRANET_USER")
login_url="$DEVC_SERVICE_UPSTREAM${DEVC_LOGIN_PATH:-/login}"
tls=()
if [ -f ca.pem ]; then
	tls=(--cacert ca.pem)
fi
jar=$(mktemp)
trap 'rm -f "$jar"' EXIT

echo "logging in to $login_url as $user" >&2
# The password is read from its file (password@file), so it never appears in
# a command line. Forms that need a CSRF token: GET the form first with
# --cookie-jar, extract the token (sed), and add it as another field.
curl --fail --silent --show-error "${tls[@]}" \
	--cookie-jar "$jar" --output /dev/null \
	--data-urlencode "username=$user" \
	--data-urlencode "password@$DEVC_SECRETS_DIR/INTRANET_PASSWORD" \
	"$login_url"

# curl's cookie file (Netscape format): domain, subdomains flag, path,
# secure flag, expiry, name, value; HttpOnly cookies carry a "#HttpOnly_"
# prefix. Values with quotes or backslashes cannot be cookie values; refuse
# them rather than print broken JSON.
awk -F '\t' '
	BEGIN { printf "["; n = 0 }
	/^#HttpOnly_/ { sub(/^#HttpOnly_/, "") }
	/^#/ || NF < 7 { next }
	{
		if ($6 ~ /["\\]/ || $7 ~ /["\\]/ || $1 ~ /["\\]/ || $3 ~ /["\\]/) {
			print "login.sh: refusing an unusable cookie" > "/dev/stderr"
			exit 1
		}
		printf "%s{\"name\":\"%s\",\"value\":\"%s\",\"domain\":\"%s\",\"path\":\"%s\",\"expiry\":%d}", (n++ ? "," : ""), $6, $7, $1, $3, $5
	}
	END { print "]"; if (n == 0) { print "login.sh: the login set no cookie (wrong password?)" > "/dev/stderr"; exit 1 } }
' "$jar"
