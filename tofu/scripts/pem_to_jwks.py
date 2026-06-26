#!/usr/bin/env python3
"""Convert an RSA public key PEM (passed via stdin as JSON) to a JWKS document.

Used as a Terraform/OpenTofu external data source.
Input (JSON on stdin):  {"public_key_pem": "-----BEGIN PUBLIC KEY-----\n..."}
Output (JSON on stdout): {"jwks": "{\"keys\":[...]}"}
"""

import base64
import hashlib
import json
import struct
import sys


def pem_to_jwks(public_key_pem: str) -> str:
    """Parse a DER-encoded RSA public key from PEM and produce a JWKS JSON string."""
    # Strip PEM headers and decode base64
    lines = [
        line
        for line in public_key_pem.strip().splitlines()
        if not line.startswith("-----")
    ]
    der = base64.b64decode("".join(lines))

    # Parse DER structure (PKCS#1 wrapped in SubjectPublicKeyInfo)
    # SubjectPublicKeyInfo ::= SEQUENCE { algorithm, subjectPublicKey BIT STRING }
    # The BIT STRING contains PKCS#1 RSAPublicKey ::= SEQUENCE { modulus INTEGER, exponent INTEGER }
    n_bytes, e_bytes = _parse_rsa_public_key_der(der)

    # Base64url encode without padding
    n_b64 = base64.urlsafe_b64encode(n_bytes).rstrip(b"=").decode()
    e_b64 = base64.urlsafe_b64encode(e_bytes).rstrip(b"=").decode()
    # Calculate kid as base64url(SHA256(DER-encoded public key)) to match Kubernetes/kind
    kid = base64.urlsafe_b64encode(hashlib.sha256(der).digest()).rstrip(b"=").decode()

    jwks = {
        "keys": [
            {
                "kty": "RSA",
                "use": "sig",
                "alg": "RS256",
                "kid": kid,
                "n": n_b64,
                "e": e_b64,
            }
        ]
    }
    return json.dumps(jwks)


def _parse_rsa_public_key_der(der: bytes) -> tuple[bytes, bytes]:
    """Extract modulus and exponent from a SubjectPublicKeyInfo DER encoding."""
    offset = 0

    def read_tag_length(data: bytes, pos: int) -> tuple[int, int, int]:
        tag = data[pos]
        pos += 1
        length = data[pos]
        pos += 1
        if length & 0x80:
            num_bytes = length & 0x7F
            length = int.from_bytes(data[pos : pos + num_bytes], "big")
            pos += num_bytes
        return tag, length, pos

    # Outer SEQUENCE (SubjectPublicKeyInfo)
    _, _, offset = read_tag_length(der, offset)

    # Skip algorithm SEQUENCE
    _, alg_len, offset = read_tag_length(der, offset)
    offset += alg_len

    # BIT STRING containing the public key
    _, _, offset = read_tag_length(der, offset)
    offset += 1  # skip the unused-bits byte

    # Inner SEQUENCE (RSAPublicKey)
    _, _, offset = read_tag_length(der, offset)

    # INTEGER (modulus)
    _, n_len, offset = read_tag_length(der, offset)
    n_bytes = der[offset : offset + n_len]
    offset += n_len
    # Strip leading zero byte if present (ASN.1 sign byte)
    if n_bytes[0] == 0:
        n_bytes = n_bytes[1:]

    # INTEGER (exponent)
    _, e_len, offset = read_tag_length(der, offset)
    e_bytes = der[offset : offset + e_len]
    if e_bytes[0] == 0:
        e_bytes = e_bytes[1:]

    return n_bytes, e_bytes


def main():
    input_data = json.load(sys.stdin)
    public_key_pem = input_data["public_key_pem"]
    jwks = pem_to_jwks(public_key_pem)
    json.dump({"jwks": jwks}, sys.stdout)


if __name__ == "__main__":
    main()
