#!/bin/zsh
# make-cert.sh [--force]
#
# Creates the self-signed certificate that partyd's HTTPS listener (port 8443)
# uses for tilt steering. Files written to party/certs/:
#   selfsigned-cert.pem   certificate
#   selfsigned-key.pem    private key (mode 600)
#
# The certificate is ECDSA P-256, valid for 397 days (iOS rejects longer
# validity), with the serverAuth EKU and a subjectAltName covering the names and
# addresses phones use, computed now: this Mac's Bonjour name (scutil --get
# LocalHostName).local, localhost, 127.0.0.1, ::1, 192.168.2.1 (macOS Internet
# Sharing) and the current IPv4 addresses of en* / bridge100. Nothing is created if a
# valid one is already present and covers the fixed names; --force regenerates it.
#
# run/party.sh prefers a real certificate at party/certs/fullchain.pem and
# party/certs/privkey.pem when those exist. This script never touches them.
set -euo pipefail

SCRIPT_DIR=${0:A:h}
CERT_DIR=$SCRIPT_DIR/certs
CRT=$CERT_DIR/selfsigned-cert.pem
KEY=$CERT_DIR/selfsigned-key.pem
OPENSSL=${OPENSSL:-openssl}
LHN=$(scutil --get LocalHostName 2>/dev/null || hostname -s)
CN=${LHN%.local}.local
SAN="DNS:$CN,DNS:localhost,IP:127.0.0.1,IP:::1,IP:192.168.2.1"
for i in bridge100 $(ifconfig -l); do
  [[ $i == (bridge100|en<->) ]] || continue
  for ip in ${(f)"$(ifconfig "$i" 2>/dev/null | awk '$1 == "inet" { print $2 }')"}; do
    [[ -n $ip && $ip != 192.168.2.1 && $SAN != *"IP:$ip,"* && $SAN != *"IP:$ip" ]] && SAN="$SAN,IP:$ip"
  done
done
DAYS=397

force=0
case ${1:-} in
  "") ;;
  --force) force=1 ;;
  *) print -u2 -- "usage: make-cert.sh [--force]"; exit 2 ;;
esac

if (( ! force )) && [[ -f $CRT && -f $KEY ]]; then
  text=$("$OPENSSL" x509 -in "$CRT" -noout -text 2>/dev/null || true)
  missing=0
  for name in "DNS:$CN" "DNS:localhost" "IP Address:127.0.0.1" "IP Address:0:0:0:0:0:0:0:1" "IP Address:192.168.2.1"; do
    [[ $text == *"$name"* ]] || missing=1
  done
  if (( ! missing )) && "$OPENSSL" x509 -in "$CRT" -noout -checkend $(( 30 * 86400 )) >/dev/null 2>&1; then
    print -r -- "certificate already present and valid: $CRT (use --force to regenerate)"
    exit 0
  fi
  print -r -- "existing certificate is missing a name or expires within 30 days; regenerating"
fi

mkdir -p "$CERT_DIR"
umask 077
"$OPENSSL" req -x509 -new -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout "$KEY" -out "$CRT" -days "$DAYS" \
  -subj "/CN=$CN" \
  -addext "subjectAltName=$SAN" \
  -addext "extendedKeyUsage=serverAuth" \
  -addext "keyUsage=critical,digitalSignature" \
  -addext "basicConstraints=critical,CA:FALSE" 2>/dev/null
chmod 600 "$KEY"
chmod 644 "$CRT"
print -r -- "wrote $CRT and $KEY"
"$OPENSSL" x509 -in "$CRT" -noout -subject -enddate
