#!/bin/sh
# Renders alertmanager.yml's ${VAR} placeholders against this
# process's own environment (env_file in docker-compose.yml) and
# starts Alertmanager on the result. A separate script, not an inline
# docker-compose command: the prom/alertmanager image is built on
# busybox, which has no envsubst (that's gettext), and a one-line sed
# pipeline with six -e flags is easy to break by accident when
# reformatted for readability inside a YAML block scalar (folding
# rules there are stricter than they look) — a real script has no such
# trap.
set -eu

sed \
  -e "s|\${ANVIL_SMTP_HOST}|$ANVIL_SMTP_HOST|g" \
  -e "s|\${ANVIL_SMTP_PORT}|$ANVIL_SMTP_PORT|g" \
  -e "s|\${ANVIL_ALERT_FROM_EMAIL}|$ANVIL_ALERT_FROM_EMAIL|g" \
  -e "s|\${ANVIL_SMTP_USERNAME}|$ANVIL_SMTP_USERNAME|g" \
  -e "s|\${ANVIL_SMTP_PASSWORD}|$ANVIL_SMTP_PASSWORD|g" \
  -e "s|\${ANVIL_ALERT_TO_EMAIL}|$ANVIL_ALERT_TO_EMAIL|g" \
  /etc/alertmanager/alertmanager.yml.template > /etc/alertmanager/alertmanager.yml

exec /bin/alertmanager --config.file=/etc/alertmanager/alertmanager.yml
