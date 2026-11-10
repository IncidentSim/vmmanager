#!/usr/bin/env bash
# Generate a dedicated private CA and distinct server/client identities (OpenSSL 3).
set -euo pipefail
umask 077
if [[ $# != 2 ]]; then
  printf 'Usage: %s NEW_PKI_DIRECTORY SERVER_DNS_NAME\n' "$0" >&2
  exit 2
fi
out=$1
host=$2
if [[ ! $host =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || [[ $host == *..* ]]; then
  printf 'Invalid server DNS name\n' >&2
  exit 2
fi
# Refuse regeneration: never silently invalidate installed client identities.
mkdir -m 700 -- "$out"
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:prime256v1 -out "$out/ca.key"
openssl req -new -x509 -sha256 -days 3650 -key "$out/ca.key" -out "$out/ca.crt" \
  -subj '/CN=Firecracker Remote Private CA' \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign'
for role in server client; do
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:prime256v1 -out "$out/$role.key"
  if [[ $role == server ]]; then
    purpose=serverAuth
    name=$host
    san="DNS:$host,DNS:localhost,IP:127.0.0.1"
  else
    purpose=clientAuth
    name=ivan-firecracker-client
    san='URI:urn:firecracker:client:ivan'
  fi
  openssl req -new -key "$out/$role.key" -out "$out/$role.csr" -subj "/CN=$name" \
    -addext 'basicConstraints=critical,CA:FALSE' \
    -addext 'keyUsage=critical,digitalSignature' \
    -addext "extendedKeyUsage=$purpose" \
    -addext "subjectAltName=$san"
  openssl x509 -req -sha256 -days 90 -in "$out/$role.csr" \
    -CA "$out/ca.crt" -CAkey "$out/ca.key" -CAcreateserial \
    -copy_extensions copy -out "$out/$role.crt"
  rm -- "$out/$role.csr"
done
chmod 600 "$out/ca.key" "$out/server.key" "$out/client.key"
printf 'Created private PKI in %s; leaf certificates expire in 90 days.\n' "$out"
printf 'Only ca.crt, client.crt and client.key belong in a client bundle.\n'
