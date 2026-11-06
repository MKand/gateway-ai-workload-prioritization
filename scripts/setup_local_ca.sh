#!/usr/bin/env bash
# Generates a self-signed Root CA and leaf certificate covering
# aiplatform.googleapis.com and *.aiplatform.googleapis.com for local Envoy or
# Internal Application Load Balancer TLS termination.
set -euo pipefail

OUT_DIR="${1:-./certs}"
DAYS="${DAYS:-365}"

mkdir -p "${OUT_DIR}"

CA_KEY="${OUT_DIR}/ca-key.pem"
CA_CERT="${OUT_DIR}/ca.pem"
SERVER_KEY="${OUT_DIR}/server-key.pem"
SERVER_CSR="${OUT_DIR}/server.csr"
SERVER_CERT="${OUT_DIR}/server.pem"
EXT_CFG="${OUT_DIR}/san.cnf"

cat > "${EXT_CFG}" <<'EOF'
[req]
distinguished_name = req_distinguished_name
req_extensions     = v3_req
prompt             = no

[req_distinguished_name]
CN = aiplatform.googleapis.com

[v3_req]
keyUsage         = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName   = @alt_names

[alt_names]
DNS.1 = aiplatform.googleapis.com
DNS.2 = *.aiplatform.googleapis.com
EOF

openssl ecparam -name prime256v1 -genkey -noout -out "${CA_KEY}"
openssl req -x509 -new -key "${CA_KEY}" -sha256 -days "${DAYS}" \
  -subj "/CN=Gemini Quota Governor Local Root CA" \
  -out "${CA_CERT}"

openssl ecparam -name prime256v1 -genkey -noout -out "${SERVER_KEY}"
openssl req -new -key "${SERVER_KEY}" -config "${EXT_CFG}" -out "${SERVER_CSR}"
openssl x509 -req -in "${SERVER_CSR}" -CA "${CA_CERT}" -CAkey "${CA_KEY}" \
  -CAcreateserial -days "${DAYS}" -sha256 \
  -extfile "${EXT_CFG}" -extensions v3_req \
  -out "${SERVER_CERT}"

rm -f "${SERVER_CSR}" "${EXT_CFG}" "${OUT_DIR}/ca.srl"
chmod 0600 "${CA_KEY}" "${SERVER_KEY}"
chmod 0644 "${CA_CERT}" "${SERVER_CERT}"

echo "Generated Root CA (${CA_CERT}) and wildcard leaf certificate (${SERVER_CERT}) in ${OUT_DIR}"
